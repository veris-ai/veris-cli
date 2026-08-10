//go:build !linux

package main

import "log/slog"

// The kernel redirect is iptables, which is netfilter, which is Linux. The
// transparent listeners still bind here, and nothing can ever route to them --
// which is why the caller warns rather than pretending this did something.
func selfSetup(*slog.Logger, setupOptions) error { return nil }
