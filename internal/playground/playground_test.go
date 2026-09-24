package playground

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

// A relay that closes with an application code says why, and the reason
// reaches whoever reads the stream instead of a bare "status = 4409".
func TestDesktopCloseCodeCarriesTheRelaysReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{Subprotocol}})
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		_ = ws.Close(4409, "another viewer took the screen")
	}))
	defer srv.Close()

	c := New(srv.URL, "veris/test")
	conn, err := c.DialDesktop(context.Background(), &Ticket{
		Ticket: "tkt_1", URL: "ws" + strings.TrimPrefix(srv.URL, "http"), Subprotocol: Subprotocol,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = io.ReadAll(conn)
	if err == nil || !strings.Contains(err.Error(), "4409") || !strings.Contains(err.Error(), "another viewer took the screen") {
		t.Errorf("read = %v, want the relay's 4409 and its reason", err)
	}
}

// A relay that echoes anything but veris-desktop -- the ticket, say -- is
// not spoken to.
func TestDesktopRefusesAnUnexpectedSubprotocol(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"tkt_1"}})
		if err != nil {
			return
		}
		_ = ws.CloseNow()
	}))
	defer srv.Close()

	_, err := New(srv.URL, "").DialDesktop(context.Background(), &Ticket{
		Ticket: "tkt_1", URL: "ws" + strings.TrimPrefix(srv.URL, "http"), Subprotocol: Subprotocol,
	})
	if err == nil || !strings.Contains(err.Error(), "subprotocol") {
		t.Errorf("DialDesktop = %v, want a refusal of the echoed subprotocol", err)
	}
}

// The refusals map to their kinds by status, and keep the bench API's words.
func TestScreenTicketRefusals(t *testing.T) {
	cases := map[int]error{
		http.StatusUnauthorized: ErrTokenRejected,
		http.StatusNotFound:     ErrSessionEnded,
		http.StatusConflict:     ErrSessionEnded,
	}
	for status, want := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"detail":"desktop not ready"}`)
		}))
		_, err := New(srv.URL, "").ScreenTicket(context.Background(), "tok")
		srv.Close()
		var e *Error
		if !errors.Is(err, want) || !errors.As(err, &e) || e.Detail != "desktop not ready" {
			t.Errorf("status %d: %v, want %v with the detail", status, err, want)
		}
	}
}
