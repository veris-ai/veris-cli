package dcvrelay

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/coder/websocket"
)

// socket relays one of the client's WebSockets: the bench relay's side is
// opened first, with the subprotocols the client offered, so the client is
// answered with the one the bench relay chose.
func (r *relay) socket(w http.ResponseWriter, req *http.Request) {
	id := int(r.ids.Add(1))
	path := req.URL.Path
	ctx := req.Context()
	up, err := r.dialUp(ctx, req)
	if err != nil {
		http.Error(w, "cannot open the Playground desktop relay", http.StatusBadGateway)
		r.closed(id, path, err)
		return
	}
	opts := &websocket.AcceptOptions{
		// Origin is a browser's defence; this listener is loopback only and
		// the native client sends whatever Origin it likes, or none.
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	}
	if p := up.Subprotocol(); p != "" {
		opts.Subprotocols = []string{p}
	}
	down, err := websocket.Accept(w, req, opts)
	if err != nil {
		_ = up.Close(websocket.StatusGoingAway, "the DCV client's handshake failed")
		r.closed(id, path, err)
		return
	}
	// DCV's display frames are large; the library's 32 KiB default would cut
	// the first one.
	up.SetReadLimit(-1)
	down.SetReadLimit(-1)
	if r.s.OnOpen != nil {
		r.s.OnOpen(id, path)
	}
	r.closed(id, path, pump(ctx, down, up))
}

func (r *relay) closed(id int, path string, err error) {
	if r.s.OnClose != nil {
		r.s.OnClose(id, path, err)
	}
}

// dialUp opens the bench relay's side of a socket under the current ticket,
// once more under a fresh ticket when the bench relay refuses the first.
func (r *relay) dialUp(ctx context.Context, req *http.Request) (*websocket.Conn, error) {
	header := http.Header{}
	if r.s.UserAgent != "" {
		header.Set("User-Agent", r.s.UserAgent)
	}
	opts := &websocket.DialOptions{
		// No whole-request timeout: it would bound the socket's life too.
		HTTPClient:      &http.Client{Transport: r.transport()},
		HTTPHeader:      header,
		Subprotocols:    offered(req),
		CompressionMode: websocket.CompressionDisabled,
	}
	for attempt := 0; ; attempt++ {
		base, err := r.base(ctx)
		if err != nil {
			return nil, err
		}
		dctx, cancel := context.WithTimeout(ctx, dialTimeout)
		up, resp, err := websocket.Dial(dctx, target(base, req), opts)
		cancel()
		if err == nil {
			return up, nil
		}
		if resp != nil && rejected(resp.StatusCode) && attempt == 0 {
			r.tickets.invalidate(base)
			continue
		}
		if resp != nil {
			return nil, fmt.Errorf("the desktop relay answered %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("cannot reach the desktop relay: %w", err)
	}
}

// offered is the subprotocols a WebSocket handshake lists, in order.
func offered(req *http.Request) []string {
	var out []string
	for _, v := range req.Header.Values("Sec-WebSocket-Protocol") {
		for p := range strings.SplitSeq(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// pump copies messages both ways until one side ends, then closes the other
// with the same close code and reason, so a client learns what the desktop
// said. The result is nil for a normal close or a relay that is stopping.
func pump(ctx context.Context, down, up *websocket.Conn) error {
	type result struct {
		fromUp bool
		err    error
	}
	done := make(chan result, 2)
	go func() { done <- result{false, copyMessages(ctx, up, down)} }()
	go func() { done <- result{true, copyMessages(ctx, down, up)} }()
	first := <-done
	from, to := down, up
	if first.fromUp {
		from, to = up, down
	}
	code, reason := closeStatus(ctx, first.err)
	_ = to.Close(code, reason)
	_ = from.CloseNow()
	<-done
	if ctx.Err() != nil {
		return nil
	}
	return closeError(first.fromUp, first.err)
}

// copyMessages writes every message read from src to dst with its type.
func copyMessages(ctx context.Context, dst, src *websocket.Conn) error {
	for {
		typ, data, err := src.Read(ctx)
		if err != nil {
			return err
		}
		if err := dst.Write(ctx, typ, data); err != nil {
			return err
		}
	}
}

// closeStatus is the close to send on after err ended the other side: its
// own code and reason when it was a close the protocol lets be sent, going
// away when the relay is stopping, an internal error when the connection
// was lost.
func closeStatus(ctx context.Context, err error) (websocket.StatusCode, string) {
	var ce websocket.CloseError
	switch {
	case errors.As(err, &ce):
		switch ce.Code {
		case websocket.StatusNoStatusRcvd, websocket.StatusAbnormalClosure, websocket.StatusTLSHandshake:
			return websocket.StatusNormalClosure, ""
		}
		return ce.Code, ce.Reason
	case ctx.Err() != nil:
		return websocket.StatusGoingAway, "the Playground screen is stopping"
	}
	return websocket.StatusInternalError, "the relay lost the connection"
}

// closeError is what ended a socket, for OnClose: nil for a normal close.
func closeError(fromUp bool, err error) error {
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		switch ce.Code {
		case websocket.StatusNormalClosure, websocket.StatusGoingAway, websocket.StatusNoStatusRcvd:
			return nil
		}
		reason := cmp.Or(ce.Reason, "no reason given")
		if fromUp {
			return fmt.Errorf("the desktop relay closed the connection (%d): %s", int(ce.Code), reason)
		}
		return fmt.Errorf("the DCV client closed the connection (%d): %s", int(ce.Code), reason)
	}
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// forward relays one resource request with its body and answers with the
// bench relay's reply.
func (r *relay) forward(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	base, err := r.base(ctx)
	if err != nil {
		http.Error(w, "the Playground session has no desktop ticket", http.StatusServiceUnavailable)
		return
	}
	out, err := http.NewRequestWithContext(ctx, req.Method, target(base, req), req.Body)
	if err != nil {
		http.Error(w, "cannot build the request", http.StatusBadRequest)
		return
	}
	out.ContentLength = req.ContentLength
	for _, h := range requestHeaders {
		if v := req.Header.Get(h); v != "" {
			out.Header.Set(h, v)
		}
	}
	if r.s.UserAgent != "" {
		out.Header.Set("User-Agent", r.s.UserAgent)
	}
	client := &http.Client{
		Transport:     r.transport(),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(out)
	if err != nil {
		http.Error(w, "cannot reach the Playground desktop relay", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if rejected(resp.StatusCode) {
		r.tickets.invalidate(base)
	}
	for _, h := range responseHeaders {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
