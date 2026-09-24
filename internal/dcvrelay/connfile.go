package dcvrelay

import (
	"fmt"
	"strings"
)

// ConsoleSession is the DCV session a Playground Windows desktop serves: the
// machine's console, which is the logged-on desktop.
const ConsoleSession = "console"

// ConnectionFile is a DCV connection file (.dcv) for the loopback relay, in
// the format of https://docs.aws.amazon.com/dcv/latest/userguide/using-connection-file.html.
type ConnectionFile struct {
	// Port is the loopback port the relay listens on.
	Port int
	// SessionID is the DCV session to open; ConsoleSession when empty.
	SessionID string
}

// Render writes the file. It pins the client to the relay the way the
// relay can serve it:
//
//   - transport=websocket: the relay carries WebSockets and HTTP only, so the
//     client skips its QUIC (UDP) attempt instead of waiting it out;
//   - webport and port both name the relay, the port auth and data share;
//   - proxytype=NONE: 127.0.0.1 is never reached through a system proxy;
//   - certificatevalidationpolicy=accept-untrusted: the relay's certificate is
//     made fresh for each run, so there is nothing a user could have trusted.
//
// No user, password or authtoken: the Playground's DCV server authenticates
// no one, the bench relay's ticket having done that, and the file holds no
// secret.
func (f ConnectionFile) Render() string {
	session := f.SessionID
	if session == "" {
		session = ConsoleSession
	}
	lines := []string{
		"[version]",
		"format=1.0",
		"",
		"[connect]",
		"host=127.0.0.1",
		fmt.Sprintf("port=%d", f.Port),
		fmt.Sprintf("webport=%d", f.Port),
		"sessionid=" + session,
		"transport=websocket",
		"proxytype=NONE",
		"certificatevalidationpolicy=accept-untrusted",
		"",
	}
	return strings.Join(lines, "\n")
}
