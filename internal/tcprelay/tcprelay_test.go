package tcprelay

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// Bytes cross both ways; the client leaving is a clean end, and both sides
// are closed once either is.
func TestPumpJoinsBothWaysAndEndsCleanlyWhenTheClientLeaves(t *testing.T) {
	client, clientFar := net.Pipe()
	up, upFar := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- Pump(client, up) }()

	go func() { _, _ = clientFar.Write([]byte("hello")) }()
	got := make([]byte, 5)
	if _, err := io.ReadFull(upFar, got); err != nil || string(got) != "hello" {
		t.Fatalf("upstream read %q, %v", got, err)
	}
	go func() { _, _ = upFar.Write([]byte("world")) }()
	if _, err := io.ReadFull(clientFar, got); err != nil || string(got) != "world" {
		t.Fatalf("client read %q, %v", got, err)
	}

	_ = clientFar.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Pump = %v, want nil when the client leaves", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Pump did not end when the client left")
	}
	if _, err := upFar.Write([]byte("x")); err == nil {
		t.Error("the upstream is still open after the client left")
	}
}

// An upstream that ends the stream without an error is ErrRelayClosed.
func TestPumpReportsAnUpstreamThatClosed(t *testing.T) {
	client, clientFar := net.Pipe()
	defer clientFar.Close()
	up, upFar := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- Pump(client, up) }()
	_ = upFar.Close()
	select {
	case err := <-done:
		if !errors.Is(err, ErrRelayClosed) {
			t.Errorf("Pump = %v, want ErrRelayClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Pump did not end when the upstream closed")
	}
}

// Serve with no handshake joins each client to a fresh upstream as is,
// and a Dial that stops the session ends Serve and closes the listener.
func TestServeRelaysRawStreamsAndStopsWhenTheSessionEnds(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ended := errors.New("session ended")
	var dials atomic.Int32
	var opened, closed atomic.Int32
	srv := &Server{
		Dial: func(context.Context) (net.Conn, error) {
			if dials.Add(1) > 1 {
				return nil, &StopError{Err: ended}
			}
			up, far := net.Pipe()
			go func() {
				defer far.Close()
				_, _ = io.Copy(far, far) // echo
			}()
			return up, nil
		},
		OnOpen:  func(int) { opened.Add(1) },
		OnClose: func(int, error) { closed.Add(1) },
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(context.Background(), ln) }()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte{0x03, 0x00, 0x00, 0x13}); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(c, got); err != nil || got[0] != 0x03 || got[3] != 0x13 {
		t.Fatalf("client read %x, %v; want its own bytes echoed", got, err)
	}
	_ = c.Close()

	c2, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	select {
	case err := <-served:
		if !errors.Is(err, ended) {
			t.Errorf("Serve = %v, want the session's end", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop when the session ended")
	}
	if opened.Load() != 2 || closed.Load() != 2 {
		t.Errorf("OnOpen %d, OnClose %d; want 2 each", opened.Load(), closed.Load())
	}
	if _, err := net.Dial("tcp", ln.Addr().String()); err == nil {
		t.Error("the listener is still open after the session ended")
	}
}
