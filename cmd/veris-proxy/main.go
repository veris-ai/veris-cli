// Command veris-proxy intercepts outbound HTTP(S) from code under test and
// routes it to simulated services in a Veris dependency sandbox.
//
// It is normally started by the Veris CLI rather than invoked directly.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/veris-ai/veris-proxy/internal/ca"
	"github.com/veris-ai/veris-proxy/internal/config"
	"github.com/veris-ai/veris-proxy/internal/proxy"
	"github.com/veris-ai/veris-proxy/internal/trust"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

const usage = `veris-proxy - route code under test at a Veris dependency sandbox

Usage:
  veris-proxy serve   --config <file> [--listen <addr>] [--transparent] [--log-level <level>]
  veris-proxy env     --config <file> [--format posix|fish|powershell|dotenv|json|github] [--explain]
  veris-proxy check   [--proxy <url>] [--expect-canary <token>] [--timeout <dur>]
  veris-proxy ca      --ca-dir <dir> [--print]
  veris-proxy version

Commands:
  serve   Run the interception proxy.
  env     Print the environment the process under test needs.
  check   Probe a running proxy and fail if interception is not live.
  ca      Create or show the local interception CA.

Exit codes:
  0  success
  1  usage or configuration error
  2  check failed: the proxy is not intercepting
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		var ce checkFailure
		if errors.As(err, &ce) {
			fmt.Fprintf(os.Stderr, "veris-proxy: %v\n", err)
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "veris-proxy: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("no command given")
	}

	switch args[0] {
	case "serve":
		return cmdServe(args[1:])
	case "env":
		return cmdEnv(args[1:])
	case "check":
		return cmdCheck(args[1:])
	case "ca":
		return cmdCA(args[1:])
	case "version", "--version", "-v":
		fmt.Println(version)
		return nil
	case "help", "--help", "-h":
		fmt.Print(usage)
		return nil
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to the proxy config (required)")
	listen := fs.String("listen", "", "override the listen address from the config")
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	logFormat := fs.String("log-format", "text", "text or json")
	transparent := fs.Bool("transparent", false,
		"also serve kernel-redirected connections (for iptables REDIRECT inside a container)")
	tHTTP := fs.String("transparent-http", "0.0.0.0:8081", "listen address for redirected plaintext traffic")
	tHTTPS := fs.String("transparent-https", "0.0.0.0:8443", "listen address for redirected TLS traffic")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("serve requires --config")
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if *listen != "" {
		cfg.Listen = *listen
		if err := cfg.Validate(); err != nil {
			return err
		}
	}

	log := newLogger(*logLevel, *logFormat)

	authority, err := ca.Load(expand(cfg.CADir))
	if err != nil {
		return err
	}

	srv := proxy.New(cfg, authority, log, version)

	// Shut down on signal so the CLI can stop us cleanly between runs.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		log.Info("shutting down")
		os.Exit(0)
	}()

	errCh := make(chan error, 2)
	if *transparent {
		// Transparent mode is what makes the container tier work: nothing in
		// the process under test has to honour HTTP_PROXY, which is the only
		// way to cover Java, static Go binaries and Apache HttpClient.
		go func() { errCh <- srv.ServeTransparent(*tHTTP, *tHTTPS) }()
	}
	go func() {
		errCh <- srv.ListenAndServe(func(addr string) {
			log.Info("veris-proxy listening",
				"addr", addr,
				"mode", string(cfg.Mode),
				"sandbox_id", cfg.SandboxID,
				"services", len(cfg.Services),
				"ca", authority.CertPath(),
				"ca_fingerprint", authority.Fingerprint(),
			)
			if cfg.Mode == config.ModePassthrough {
				log.Warn("mode=passthrough: unmapped hosts reach the real internet. " +
					"A passing test run does not prove interception.")
			}
		})
	}()
	return <-errCh
}

func cmdEnv(args []string) error {
	fs := flag.NewFlagSet("env", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to the proxy config (required)")
	format := fs.String("format", "posix", "posix, fish, powershell, dotenv, json or github")
	explain := fs.Bool("explain", false, "annotate each variable with why it is set")
	quiet := fs.Bool("quiet", false, "suppress coverage warnings on stderr")
	javaStore := fs.String("java-truststore", "", "path to a JKS truststore containing the Veris CA")
	javaPass := fs.String("java-truststore-pass", "changeit", "password for the JKS truststore")
	proxyURL := fs.String("proxy-url", "", "override the proxy URL (defaults to the config listen address)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("env requires --config")
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	authority, err := ca.Load(expand(cfg.CADir))
	if err != nil {
		return err
	}

	url := *proxyURL
	if url == "" {
		url = "http://" + cfg.Listen
	}

	opts := trust.Options{
		ProxyURL:           url,
		CACertPath:         authority.CertPath(),
		JavaTrustStore:     *javaStore,
		JavaTrustStorePass: *javaPass,
		SandboxID:          cfg.SandboxID,
		CanaryToken:        cfg.CanaryToken,
		NoProxy:            cfg.AllowPassthrough,
	}

	if err := trust.Format(os.Stdout, trust.Build(opts), *format, *explain); err != nil {
		return err
	}

	// Warnings go to stderr so `eval "$(veris-proxy env --config ...)"` stays
	// clean while the developer still sees them.
	if !*quiet {
		for _, warning := range trust.Warnings(opts) {
			fmt.Fprintf(os.Stderr, "warning: %s\n", warning)
		}
	}
	return nil
}

// checkFailure marks an interception probe failure, which exits 2 rather than 1
// so a test harness can tell it apart from a usage error.
type checkFailure struct{ error }

func cmdCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	proxyURL := fs.String("proxy", "", "proxy URL (defaults to $VERIS_PROXY_URL)")
	expect := fs.String("expect-canary", "", "canary token that must match (defaults to $VERIS_CANARY)")
	timeout := fs.Duration("timeout", 5*time.Second, "probe timeout")
	quiet := fs.Bool("quiet", false, "print nothing on success")
	if err := fs.Parse(args); err != nil {
		return err
	}

	url := *proxyURL
	if url == "" {
		url = os.Getenv("VERIS_PROXY_URL")
	}
	if url == "" {
		return checkFailure{errors.New("no proxy URL: pass --proxy or set VERIS_PROXY_URL. " +
			"If you expected the environment to be set, interception is not active")}
	}

	want := *expect
	if want == "" {
		want = os.Getenv("VERIS_CANARY")
	}

	client := &http.Client{Timeout: *timeout}
	resp, err := client.Get(url + proxy.StatusPath)
	if err != nil {
		return checkFailure{fmt.Errorf("cannot reach the proxy at %s: %w", url, err)}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return checkFailure{fmt.Errorf("read probe response: %w", err)}
	}
	if resp.StatusCode != http.StatusOK {
		return checkFailure{fmt.Errorf("proxy returned %d: %s", resp.StatusCode, string(body))}
	}

	var state struct {
		VerisProxy  bool   `json:"veris_proxy"`
		SandboxID   string `json:"sandbox_id"`
		Mode        string `json:"mode"`
		CanaryToken string `json:"canary_token"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		return checkFailure{fmt.Errorf("response from %s is not a Veris proxy: %w", url, err)}
	}
	if !state.VerisProxy {
		return checkFailure{fmt.Errorf("something is listening at %s but it is not a Veris proxy", url)}
	}
	if want != "" && state.CanaryToken != want {
		// This is the case worth having: a proxy left running from an earlier
		// run, pointing at a different sandbox. Without the token check the
		// tests would pass against the wrong simulated data.
		return checkFailure{fmt.Errorf(
			"canary mismatch: the proxy at %s belongs to a different run (sandbox %s). "+
				"Stop it and restart with the current config", url, state.SandboxID)}
	}
	if state.Mode != string(config.ModeStrict) {
		fmt.Fprintf(os.Stderr,
			"warning: proxy is in %s mode, so unmapped hosts still reach the real internet\n", state.Mode)
	}

	if !*quiet {
		fmt.Printf("interception live: sandbox %s, mode %s\n", state.SandboxID, state.Mode)
	}
	return nil
}

func cmdCA(args []string) error {
	fs := flag.NewFlagSet("ca", flag.ContinueOnError)
	dir := fs.String("ca-dir", defaultCADir(), "directory holding the CA")
	print := fs.Bool("print", false, "write the CA certificate to stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}

	authority, err := ca.Load(expand(*dir))
	if err != nil {
		return err
	}
	if *print {
		_, err := os.Stdout.Write(authority.CertPEM())
		return err
	}

	out, _ := json.MarshalIndent(map[string]any{
		"path":        authority.CertPath(),
		"dir":         authority.Dir(),
		"fingerprint": authority.Fingerprint(),
		"expires":     authority.NotAfter().Format(time.RFC3339),
	}, "", "  ")
	fmt.Println(string(out))
	return nil
}

func defaultCADir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".veris/ca"
	}
	return filepath.Join(home, ".veris", "ca")
}

func expand(path string) string {
	if path == "" {
		return defaultCADir()
	}
	if path == "~" || len(path) > 1 && path[:2] == "~/" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[1:])
		}
	}
	return path
}

func newLogger(level, format string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: lvl}
	// Logs go to stderr so stdout stays free for machine-readable output.
	if format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}
