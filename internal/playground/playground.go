// Package playground is the CLI's client for a bench Playground's screen:
// the bench API's world-ssh routes that redeem a one-time connect code and
// issue desktop tickets, and the WebSocket that carries the desktop's RFB
// stream.
//
// The bench API is a different service from the control plane the rest of
// the CLI talks to, and nothing here reads a login, profile or API key: the
// connect code is the whole credential, and the session token it redeems for
// is held in memory only and never printed.
package playground

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/veris-ai/veris-cli/internal/direct"
)

// Subprotocol is the WebSocket subprotocol the desktop relay speaks and
// echoes back.
const Subprotocol = "veris-desktop"

// maxBody bounds what is read of any answer; every real one is a few
// hundred bytes.
const maxBody = 1 << 20

// dialTimeout bounds the WebSocket handshake with the desktop relay.
const dialTimeout = 30 * time.Second

// The ways a Playground refuses, matched with errors.Is on an *Error.
var (
	// ErrCodeRejected is a connect code the bench API does not know, has
	// expired, or has already redeemed.
	ErrCodeRejected = errors.New("connect code rejected")
	// ErrTokenRejected is a session token the bench API no longer accepts.
	ErrTokenRejected = errors.New("the Playground session's token is no longer valid")
	// ErrSessionEnded is a Playground session that has ended or whose
	// desktop is not ready.
	ErrSessionEnded = errors.New("the Playground session has ended")
)

// Error is a non-2xx answer from the bench API.
type Error struct {
	Status int
	// Detail is the answer's `detail` string, empty when it had none.
	Detail string
	kind   error
}

// Error renders "[404] connect code not found". The URL is left out, as
// the control plane client leaves it out: the line is shown to people.
func (e *Error) Error() string {
	msg := e.Detail
	if msg == "" {
		msg = strings.ToLower(http.StatusText(e.Status))
	}
	return fmt.Sprintf("[%d] %s", e.Status, msg)
}

// Unwrap lets errors.Is match the refusal's kind, one of the Err* above.
func (e *Error) Unwrap() error { return e.kind }

// Client talks to one bench API.
type Client struct {
	// Base is the bench API's origin, no trailing slash.
	Base string
	// HTTP is the transport; New gives it a 30 s timeout and a transport
	// that never goes through a veris proxy.
	HTTP *http.Client
	// UserAgent names the binary and version to the bench API's logs.
	UserAgent string
}

// New returns a client for base. A trailing slash is trimmed so paths can
// be appended verbatim.
func New(base, userAgent string) *Client {
	return &Client{
		Base:      strings.TrimSuffix(base, "/"),
		HTTP:      &http.Client{Timeout: 30 * time.Second, Transport: direct.Transport()},
		UserAgent: userAgent,
	}
}

// Session is what a connect code redeems for. Token authorises desktop
// tickets for this session; it is a secret and is never printed.
type Session struct {
	WorldID   string `json:"world_id"`
	SessionID string `json:"session_id"`
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

// Ticket admits one WebSocket connection to the desktop relay at URL, for
// about a minute. Ticket is a secret and is never printed.
type Ticket struct {
	Ticket      string `json:"ticket"`
	URL         string `json:"url"`
	Subprotocol string `json:"subprotocol"`
	ExpiresAt   string `json:"expires_at"`
}

// Redeem exchanges a one-time connect code for a session. A 404 is an
// *Error matching ErrCodeRejected.
func (c *Client) Redeem(ctx context.Context, code string) (*Session, error) {
	var s Session
	err := c.post(ctx, "/v1/world-ssh/connect", "", map[string]string{"code": code}, &s,
		map[int]error{http.StatusNotFound: ErrCodeRejected})
	if err != nil {
		return nil, err
	}
	if s.Token == "" {
		return nil, errors.New("the bench API redeemed the code but returned no session token")
	}
	return &s, nil
}

// ScreenTicket asks for a fresh desktop ticket under token. A 401 matches
// ErrTokenRejected; a 404 or 409 matches ErrSessionEnded.
func (c *Client) ScreenTicket(ctx context.Context, token string) (*Ticket, error) {
	var t Ticket
	err := c.post(ctx, "/v1/world-ssh/screen-ticket", token, nil, &t, map[int]error{
		http.StatusUnauthorized: ErrTokenRejected,
		http.StatusNotFound:     ErrSessionEnded,
		http.StatusConflict:     ErrSessionEnded,
	})
	if err != nil {
		return nil, err
	}
	if t.URL == "" || t.Ticket == "" {
		return nil, errors.New("the bench API issued a desktop ticket with no url or ticket")
	}
	if t.Subprotocol == "" {
		t.Subprotocol = Subprotocol
	}
	return &t, nil
}

// DialDesktop opens the desktop relay's WebSocket with t and returns it as
// a byte stream of RFB. The ticket rides the Sec-WebSocket-Protocol header
// as the second subprotocol, after the one the relay echoes. A close code
// of 4000 or above ends a Read with an error carrying the relay's reason.
func (c *Client) DialDesktop(ctx context.Context, t *Ticket) (net.Conn, error) {
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	header := http.Header{}
	if c.UserAgent != "" {
		header.Set("User-Agent", c.UserAgent)
	}
	// The dial's own client, without the 30 s whole-request timeout: that
	// would also bound the upgraded connection's life. dctx bounds the
	// handshake alone.
	hc := &http.Client{Transport: direct.Transport()}
	if c.HTTP != nil && c.HTTP.Transport != nil {
		hc.Transport = c.HTTP.Transport
	}
	ws, _, err := websocket.Dial(dctx, t.URL, &websocket.DialOptions{
		HTTPClient:   hc,
		HTTPHeader:   header,
		Subprotocols: []string{t.Subprotocol, t.Ticket},
	})
	if err != nil {
		return nil, fmt.Errorf("cannot open the desktop relay: %w", err)
	}
	if got := ws.Subprotocol(); got != t.Subprotocol {
		_ = ws.Close(websocket.StatusProtocolError, "unexpected subprotocol")
		return nil, fmt.Errorf("the desktop relay answered with subprotocol %q, not %q", got, t.Subprotocol)
	}
	// RFB framebuffer updates arrive as messages of any size; the library's
	// default 32 KiB limit would cut the first large one.
	ws.SetReadLimit(-1)
	return &desktopConn{Conn: websocket.NetConn(context.Background(), ws, websocket.MessageBinary)}, nil
}

// desktopConn is the relay's stream, with its application close codes made
// readable.
type desktopConn struct{ net.Conn }

func (d *desktopConn) Read(p []byte) (int, error) {
	n, err := d.Conn.Read(p)
	var ce websocket.CloseError
	if err != nil && errors.As(err, &ce) && ce.Code >= 4000 {
		reason := ce.Reason
		if reason == "" {
			reason = "no reason given"
		}
		err = fmt.Errorf("the desktop relay closed the connection (%d): %s", int(ce.Code), reason)
	}
	return n, err
}

// post sends one JSON POST and decodes a 2xx answer into out. token, when
// set, is sent as a Bearer credential. A non-2xx answer is an *Error whose
// kind comes from kinds by status.
func (c *Client) post(ctx context.Context, path, token string, in, out any, kinds map[int]error) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encode POST %s: %w", path, err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach the bench API at %s: %w", c.Base, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("read POST %s: %w", path, err)
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Detail any `json:"detail"`
		}
		apiErr := &Error{Status: resp.StatusCode, kind: kinds[resp.StatusCode]}
		if json.Unmarshal(raw, &e) == nil {
			if s, ok := e.Detail.(string); ok {
				apiErr.Detail = s
			}
		}
		return apiErr
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode POST %s: %w", path, err)
	}
	return nil
}
