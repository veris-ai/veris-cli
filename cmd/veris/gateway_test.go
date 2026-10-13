package main

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/veris-ai/veris-cli/internal/cli"
)

func TestGatewayForwardRefusesBeforeListening(t *testing.T) {
	dir := t.TempDir()
	upstream := filepath.Join(dir, "upstream")
	if err := os.WriteFile(upstream, []byte("http://v1.sbx:sbx@gw.example:8080\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		opts    gatewayForwardOptions
		wantErr string
	}{
		{"no listen", gatewayForwardOptions{UpstreamFile: upstream}, "--listen is required"},
		{"no upstream file", gatewayForwardOptions{Listen: "127.0.0.1:0"}, "--upstream-file is required"},
		{"all interfaces", gatewayForwardOptions{Listen: "0.0.0.0:0", UpstreamFile: upstream}, "not a loopback address"},
		{"a LAN address", gatewayForwardOptions{Listen: "192.168.1.10:3128", UpstreamFile: upstream}, "not a loopback address"},
		{"missing upstream file", gatewayForwardOptions{Listen: "127.0.0.1:0", UpstreamFile: filepath.Join(dir, "nope")}, "read the upstream file"},
		{"deny on a LAN address", gatewayForwardOptions{Listen: "127.0.0.1:0", DenyListen: "192.168.1.10:3128", UpstreamFile: upstream}, "--deny-listen"},
		{"missing PAC file", gatewayForwardOptions{Listen: "127.0.0.1:0", UpstreamFile: upstream, PACFile: filepath.Join(dir, "nope.pac")}, "--pac-file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := gatewayForward(tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), "v1.sbx:sbx") {
				t.Errorf("the error quotes the credential: %v", err)
			}
		})
	}
}

func TestGatewayForwardHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := cli.Execute(root(), &cli.Globals{}, []string{"gateway", "forward", "--help"}, &stdout, &stderr)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("err = %v, want flag.ErrHelp", err)
	}
	for _, want := range []string{
		"veris gateway forward - ",
		"--listen 127.0.0.1:<port> --upstream-file <path> [--pac-file <path>] [--deny-listen 127.0.0.1:<port>]",
		"  --listen ", "  --upstream-file ", "  --pac-file ", "  --deny-listen ", "/proxy.pac",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("help lacks %q:\n%s", want, stdout.String())
		}
	}
}
