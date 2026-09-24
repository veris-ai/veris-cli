package vncrelay

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
)

// ErrRelayClosed is a desktop relay that ended the stream while the viewer
// was still connected, without saying why.
var ErrRelayClosed = errors.New("the desktop relay closed the connection")

// StopError wraps an error from Server.Dial after which no later viewer can
// be served either -- the session behind the relay has ended. Serve closes
// the listener, disconnects every viewer and returns it.
type StopError struct{ Err error }

func (e *StopError) Error() string { return e.Err.Error() }
func (e *StopError) Unwrap() error { return e.Err }

// Server relays each viewer that connects to its listener to an upstream of
// its own. Viewers are independent: each gets a fresh Dial.
type Server struct {
	// Password is the one-time password every viewer must answer with.
	Password string
	// Dial opens a fresh upstream RFB stream for one viewer. Wrap the error
	// in a *StopError when no later Dial could succeed either.
	Dial func(ctx context.Context) (net.Conn, error)
	// OnOpen, when set, is told a viewer connected; id numbers viewers from 1.
	OnOpen func(id int)
	// OnClose, when set, is told a viewer's connection ended, and why: nil
	// when the viewer left, otherwise what ended it.
	OnClose func(id int, err error)
}

// Serve accepts viewers on ln until ctx is done or a Dial returns a
// *StopError, and closes ln either way. It returns nil when ctx ended it,
// the *StopError when the session did, and an Accept failure otherwise;
// every viewer's connection is closed before it returns.
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

// relay serves one viewer: a fresh upstream, both handshakes, then the two
// streams joined until either side ends. A connection cut short because
// Serve is stopping reports nil.
func (s *Server) relay(ctx context.Context, viewer net.Conn) error {
	defer viewer.Close()
	defer context.AfterFunc(ctx, func() { _ = viewer.Close() })()

	up, err := s.Dial(ctx)
	if err != nil {
		return err
	}
	defer up.Close()
	defer context.AfterFunc(ctx, func() { _ = up.Close() })()

	err = UpstreamHandshake(up)
	if err == nil {
		err = ViewerHandshake(viewer, s.Password, nil)
	}
	if err == nil {
		err = pump(viewer, up)
	}
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// pump copies both ways until one direction ends, then closes both. What
// the relay sent past its handshake is already waiting on up and reaches
// the viewer first. A viewer that left is nil; a relay that ended the
// stream is its error, or ErrRelayClosed when it gave none.
func pump(viewer, up net.Conn) error {
	type result struct {
		fromRelay bool
		err       error
	}
	done := make(chan result, 2)
	go func() {
		_, err := io.Copy(up, viewer)
		done <- result{false, err}
	}()
	go func() {
		_, err := io.Copy(viewer, up)
		done <- result{true, err}
	}()
	first := <-done
	_ = viewer.Close()
	_ = up.Close()
	<-done
	if first.fromRelay && first.err == nil {
		return ErrRelayClosed
	}
	return first.err
}
