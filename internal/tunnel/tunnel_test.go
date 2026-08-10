//go:build unix

package tunnel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeCloudflared writes a stand-in onto PATH so these tests exercise the
// supervision without opening a real tunnel. The real thing is driven by
// testdata/e2e-ingress.sh.
func fakeCloudflared(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cloudflared")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestStartReturnsTheAnnouncedHostname(t *testing.T) {
	fakeCloudflared(t, `
echo "2026-08-10T12:00:00Z INF Requesting new quick Tunnel" >&2
echo "+---------------------------------------+" >&2
echo "|  https://odd-forest-1a2b.trycloudflare.com  |" >&2
echo "+---------------------------------------+" >&2
sleep 30
`)
	tun, err := Start(context.Background(), Options{Target: "http://127.0.0.1:1234"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer tun.Stop()

	if tun.URL() != "https://odd-forest-1a2b.trycloudflare.com" {
		t.Errorf("URL() = %q", tun.URL())
	}
}

// A tunnel whose hostname we never read is useless, and reporting it ready
// would be the silent success this whole tier exists to refuse.
func TestATunnelThatAnnouncesNothingIsAFailure(t *testing.T) {
	fakeCloudflared(t, `echo "INF starting" >&2; exit 3`)

	_, err := Start(context.Background(), Options{Target: "http://127.0.0.1:1234"})
	if err == nil {
		t.Fatal("a cloudflared that exits without a hostname must not report success")
	}
	if !strings.Contains(err.Error(), "before announcing a hostname") {
		t.Errorf("the error should say what was missing: %v", err)
	}
}

func TestAMissingBinaryNamesTheRemedy(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	_, err := Start(context.Background(), Options{Target: "http://127.0.0.1:1234"})
	if err == nil {
		t.Fatal("expected a refusal when cloudflared is absent")
	}
	for _, want := range []string{"not on PATH", "install", "--expose"} {
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(want)) {
			t.Errorf("the refusal should mention %q: %v", want, err)
		}
	}
}

// A named tunnel prints no announcement, so a token without a hostname would
// leave us registering an empty destination.
func TestANamedTunnelDemandsItsHostname(t *testing.T) {
	fakeCloudflared(t, `sleep 30`)

	_, err := Start(context.Background(), Options{
		Target: "http://127.0.0.1:1234", Token: "tok",
	})
	if err == nil || !strings.Contains(err.Error(), "hostname") {
		t.Fatalf("a token without a hostname should be refused: %v", err)
	}

	tun, err := Start(context.Background(), Options{
		Target: "http://127.0.0.1:1234", Token: "tok", Hostname: "https://hooks.example.com",
	})
	if err != nil {
		t.Fatalf("Start with a hostname: %v", err)
	}
	defer tun.Stop()
	if tun.URL() != "https://hooks.example.com" {
		t.Errorf("URL() = %q, want the configured hostname", tun.URL())
	}
}

// The announcement is a URL, not a bare domain: prose mentioning the domain
// must not be mistaken for it.
func TestOnlyASchemedURLCountsAsTheAnnouncement(t *testing.T) {
	fakeCloudflared(t, `
echo "INF Your quick tunnel will use trycloudflare.com shortly" >&2
sleep 0.3
echo "INF |  https://real-one-9z8y.trycloudflare.com  |" >&2
sleep 30
`)
	tun, err := Start(context.Background(), Options{Target: "http://127.0.0.1:1234"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer tun.Stop()
	if tun.URL() != "https://real-one-9z8y.trycloudflare.com" {
		t.Errorf("URL() = %q", tun.URL())
	}
}

// A tunnel dying mid-run has to be observable, or the first sign is missing
// callbacks with nothing to explain them.
func TestDoneClosesWhenTheTunnelDies(t *testing.T) {
	fakeCloudflared(t, `
echo "|  https://short-lived-1a2b.trycloudflare.com  |" >&2
sleep 0.2
exit 1
`)
	tun, err := Start(context.Background(), Options{Target: "http://127.0.0.1:1234"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer tun.Stop()

	select {
	case <-tun.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done never closed after cloudflared exited")
	}
}

// A tunnel that ignores SIGTERM must still be stopped: Stop runs once, and a
// survivor keeps a public URL answering for an app that is gone.
func TestStopEscalatesToKillWhenSIGTERMIsIgnored(t *testing.T) {
	fakeCloudflared(t, `
trap '' TERM
echo "|  https://stubborn-1a2b.trycloudflare.com  |" >&2
while true; do sleep 0.2; done
`)
	tun, err := Start(context.Background(), Options{Target: "http://127.0.0.1:1234"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	start := time.Now()
	_ = tun.Stop()

	select {
	case <-tun.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the tunnel survived Stop, so its public URL is still answering")
	}
	if elapsed := time.Since(start); elapsed > 3*gracePeriod {
		t.Errorf("Stop took %s", elapsed)
	}
}

// The flag asks for a hostname, and a hostname is not a callback destination:
// the sandbox refuses anything without an https scheme.
func TestABareNamedTunnelHostnameBecomesAnHTTPSOrigin(t *testing.T) {
	fakeCloudflared(t, `sleep 30`)
	tun, err := Start(context.Background(), Options{
		Target: "http://127.0.0.1:1234", Token: "tok", Hostname: "hooks.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Stop()
	if tun.URL() != "https://hooks.example.com" {
		t.Errorf("URL() = %q, want an https origin", tun.URL())
	}
}
