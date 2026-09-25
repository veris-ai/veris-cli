// Package tcprelay puts a remote byte stream on a local port: every TCP
// connection accepted gets an upstream of its own, dialled fresh, and the two
// are joined until either side ends. It is the part a VNC viewer and a Remote
// Desktop client share; a protocol that must complete a handshake on either
// side first does so in Server.Handshake.
package tcprelay

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
)

// ErrRelayClosed is an upstream that ended the stream while the local
// client was still connected, without saying why.
var ErrRelayClosed = errors.New("the desktop relay closed the connection")

// StopError wraps an error from Server.Dial after which no later client can
// be served either -- the session behind the relay has ended. Serve closes
// the listener, disconnects every client and returns it.
type StopError struct{ Err error }

func (e *StopError) Error() string { return e.Err.Error() }
func (e *StopError) Unwrap() error { return e.Err }

// Server relays each client that connects to its listener to an upstream of
// its own. Clients are independent: each gets a fresh Dial.
type Server struct {
	// Dial opens a fresh upstream stream for one client. Wrap the error in
	// a *StopError when no later Dial could succeed either.
	Dial func(ctx context.Context) (net.Conn, error)
	// Handshake, when set, runs on both streams before they are joined.
	Handshake func(client, up net.Conn) error
	// OnOpen, when set, is told a client connected; id numbers clients from 1.
	OnOpen func(id int)
	// OnClose, when set, is told a client's connection ended, and why: nil
	// when the client left, otherwise what ended it.
	OnClose func(id int, err error)
}

// Serve accepts clients on ln until ctx is done or a Dial returns a
// *StopError, and closes ln either way. It returns nil when ctx ended it,
// the *StopError when the session did, and an Accept failure otherwise;
// every client's connection is closed before it returns.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	ctx, cancel := context.WithCancelCause(ctx)
	var wg sync.WaitGroup
	defer func() {
		cancel(nil)
		wg.Wait()
	}()
	context.AfterFunc(ctx, func() { _ = ln.Close() })

	for id := 1; ; id++ {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() == nil {
				return err
			}
			var stop *StopError
			if errors.As(context.Cause(ctx), &stop) {
				return stop
			}
			return nil
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.OnOpen != nil {
				s.OnOpen(id)
			}
			err := s.relay(ctx, conn)
			if s.OnClose != nil {
				s.OnClose(id, err)
			}
			var stop *StopError
			if errors.As(err, &stop) {
				cancel(stop)
			}
		}()
	}
}

// relay serves one client: a fresh upstream, the handshake if any, then the
// two streams joined until either side ends. A connection cut short because
// Serve is stopping reports nil.
func (s *Server) relay(ctx context.Context, client net.Conn) error {
	defer client.Close()
	defer context.AfterFunc(ctx, func() { _ = client.Close() })()

	up, err := s.Dial(ctx)
	if err != nil {
		return err
	}
	defer up.Close()
	defer context.AfterFunc(ctx, func() { _ = up.Close() })()

	if s.Handshake != nil {
		err = s.Handshake(client, up)
	}
	if err == nil {
		err = Pump(client, up)
	}
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// Pump copies both ways until one direction ends, then closes both. What
// the upstream sent past a handshake is already waiting on up and reaches
// the client first. A client that left is nil; an upstream that ended the
// stream is its error, or ErrRelayClosed when it gave none.
func Pump(client, up net.Conn) error {
	type result struct {
		fromUpstream bool
		err          error
	}
	done := make(chan result, 2)
	go func() {
		_, err := io.Copy(up, client)
		done <- result{false, err}
	}()
	go func() {
		_, err := io.Copy(client, up)
		done <- result{true, err}
	}()
	first := <-done
	_ = client.Close()
	_ = up.Close()
	<-done
	if first.fromUpstream && first.err == nil {
		return ErrRelayClosed
	}
	return first.err
}
