// Package hostport holds the one piece of host parsing every layer needs.
//
// It existed three times -- in ca, config and proxy -- with the same body and
// the same flaw: net.SplitHostPort signals "no port" by returning an error,
// and building that error allocates. A bare hostname is the common case on the
// transparent listener, where SNI and the Host header carry no port.
package hostport

import (
	"net"
	"strings"
)

// StripPort returns host without a trailing :port, and host unchanged when
// there is none.
func StripPort(host string) string {
	if strings.IndexByte(host, ':') < 0 {
		return host
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}
