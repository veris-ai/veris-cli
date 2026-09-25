package vncrelay

import (
	"context"
	"net"

	"github.com/veris-ai/veris-cli/internal/tcprelay"
)

// ErrRelayClosed is a desktop relay that ended the stream while the viewer
// was still connected, without saying why.
var ErrRelayClosed = tcprelay.ErrRelayClosed

// StopError wraps an error from Server.Dial after which no later viewer can
// be served either -- the session behind the relay has ended. Serve closes
// the listener, disconnects every viewer and returns it.
type StopError = tcprelay.StopError

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
// every viewer's connection is closed before it returns. Each viewer and
// its upstream complete their RFB handshakes before the streams are joined.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	relay := &tcprelay.Server{
		Dial: s.Dial,
		Handshake: func(viewer, up net.Conn) error {
			if err := UpstreamHandshake(up); err != nil {
				return err
			}
			return ViewerHandshake(viewer, s.Password, nil)
		},
		OnOpen:  s.OnOpen,
		OnClose: s.OnClose,
	}
	return relay.Serve(ctx, ln)
}
