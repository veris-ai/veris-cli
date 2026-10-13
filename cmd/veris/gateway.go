package main

import (
	"context"
	"errors"
	"flag"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/veris-ai/veris-cli/internal/cli"
	"github.com/veris-ai/veris-cli/internal/gateway"
)

// gatewayCommand is the gateway group: the local side of a Playground
// desktop's route through the Veris egress gateway.
func gatewayCommand() *cli.Command {
	return &cli.Command{
		Name:    "gateway",
		Summary: "Reach the Veris egress gateway from apps that cannot carry its credential",
		Sub:     []*cli.Command{gatewayForwardCommand()},
	}
}

// gatewayForwardHelp is forward's --help prose.
const gatewayForwardHelp = `The egress gateway is an HTTP CONNECT proxy that refuses a tunnel without
Basic Proxy-Authorization naming the sandbox. Operating-system proxy settings
-- a host and port, or a PAC file -- cannot carry that credential, so GUI apps
cannot use the gateway directly. forward listens on loopback, asks for no
credential, and opens each CONNECT tunnel through the gateway with one.

It is a pure relay: no TLS is terminated and nothing inside a tunnel is read.
Only CONNECT is served; any other request is answered 501, except GET
/proxy.pac when --pac-file is given, which serves that file (read afresh per
request) so the system setting can point at
http://127.0.0.1:<port>/proxy.pac.

--deny-listen serves a second loopback port that answers every request 403
and opens nothing, for a PAC file to name for hosts that must not be reached
at all. A closed port will not do: macOS treats an unreachable proxy as a
failed one and connects directly.

--upstream-file holds the gateway's URL, http://<user>:<pass>@<host>:<port>,
and nothing else; it carries a credential, so it should be mode 0600, and
neither it nor the credential is ever logged -- only the gateway's host:port.
A gateway that refuses a tunnel has its status (403, 407, 502...) relayed to
the app; one that cannot be reached is answered 502.

Runs until SIGINT or SIGTERM, logging one line as each tunnel opens and one
as it closes, with its target, the gateway's status, bytes and duration.`

// gatewayForwardCommand is `veris gateway forward`.
func gatewayForwardCommand() *cli.Command {
	var listen, denyListen, upstreamFile, pacFile, logLevel, logFormat string
	return &cli.Command{
		Name:    "forward",
		Summary: "Serve CONNECT on loopback and tunnel through the gateway with its credential",
		Usage: "veris gateway forward --listen 127.0.0.1:<port> --upstream-file <path> " +
			"[--pac-file <path>] [--deny-listen 127.0.0.1:<port>]",
		Help: gatewayForwardHelp,
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&listen, "listen", "", "loopback `address` to serve CONNECT on, e.g. 127.0.0.1:3128")
			fs.StringVar(&upstreamFile, "upstream-file", "",
				"`file` holding the gateway's proxy URL, credential included (mode 0600)")
			fs.StringVar(&pacFile, "pac-file", "", "serve this `file` at GET /proxy.pac")
			fs.StringVar(&denyListen, "deny-listen", "", "loopback `address` that answers every request 403")
			fs.StringVar(&logLevel, "log-level", "info", "debug, info, warn or error")
			fs.StringVar(&logFormat, "log-format", "text", "text or json")
		},
		Run: func(ctx *cli.Context, args []string) error {
			if err := noPositionals(ctx, args); err != nil {
				return err
			}
			return gatewayForward(gatewayForwardOptions{
				Listen: listen, DenyListen: denyListen, UpstreamFile: upstreamFile, PACFile: pacFile,
				LogLevel: logLevel, LogFormat: logFormat,
			})
		},
	}
}

// gatewayForwardOptions are forward's flags.
type gatewayForwardOptions struct {
	Listen, DenyListen, UpstreamFile, PACFile string
	LogLevel, LogFormat                       string
}

// gatewayForward runs the relay until a signal, then shuts it down.
func gatewayForward(o gatewayForwardOptions) error {
	if o.Listen == "" {
		return &cli.UsageError{Msg: "--listen is required (a loopback address, e.g. 127.0.0.1:3128)"}
	}
	if o.UpstreamFile == "" {
		return &cli.UsageError{Msg: "--upstream-file is required"}
	}
	if err := gateway.CheckLoopback(o.Listen); err != nil {
		return err
	}
	if o.DenyListen != "" {
		if err := gateway.CheckLoopback(o.DenyListen); err != nil {
			return errors.New("--deny-listen: " + err.Error())
		}
	}
	log := newLogger(o.LogLevel, o.LogFormat)
	upstream, err := gateway.ReadUpstreamFile(o.UpstreamFile, log.Warn)
	if err != nil {
		return err
	}
	if o.PACFile != "" {
		if _, err := os.Stat(o.PACFile); err != nil {
			return errors.New("--pac-file: " + err.Error())
		}
	}
	ln, err := net.Listen("tcp", o.Listen)
	if err != nil {
		return err
	}
	fwd := &gateway.Forwarder{Upstream: upstream, PACFile: o.PACFile, Log: log}
	var deny *gateway.Forwarder
	if o.DenyListen != "" {
		denyLn, err := net.Listen("tcp", o.DenyListen)
		if err != nil {
			_ = ln.Close()
			return errors.New("--deny-listen: " + err.Error())
		}
		deny = &gateway.Forwarder{Deny: true, Log: log}
		go func() {
			if err := deny.Serve(denyLn); err != nil {
				log.Error("the deny listener stopped", "err", err)
			}
		}()
		log.Info("gateway deny listening", "addr", denyLn.Addr().String())
	}

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		<-sigCtx.Done()
		log.Info("shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if deny != nil {
			_ = deny.Shutdown(ctx)
		}
		_ = fwd.Shutdown(ctx)
	}()

	log.Info("gateway forward listening", "addr", ln.Addr().String(),
		"upstream", upstream.Addr, "pac", o.PACFile != "")
	if err := fwd.Serve(ln); err != nil {
		return err
	}
	// Serve returns as soon as the listener closes; the tunnels still open
	// get the grace period before the process exits under them.
	<-drained
	return nil
}
