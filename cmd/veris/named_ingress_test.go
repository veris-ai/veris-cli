package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/veris-ai/veris-cli/internal/callback"
	"github.com/veris-ai/veris-cli/internal/config"
	"github.com/veris-ai/veris-cli/internal/proxy"
)

// The stand-in forwards according to the remote service URL, independently of
// cloudflared's local argv. This is the named-tunnel configuration contract.
func TestNamedIngressRemoteDestination(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell tunnel stand-in")
	}
	for _, pendingMode := range []bool{false, true} {
		for _, recorderRoute := range []bool{false, true} {
			name := "existing"
			if pendingMode {
				name = "precreated"
			}
			if recorderRoute {
				name += "/recorder"
			} else {
				name += "/app-bypass"
			}
			t.Run(name, func(t *testing.T) {
				app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
				defer app.Close()
				host, portText, _ := net.SplitHostPort(strings.TrimPrefix(app.URL, "http://"))
				port, _ := strconv.Atoi(portText)
				binary := filepath.Join(t.TempDir(), "cloudflared")
				if err := os.WriteFile(binary, []byte("#!/bin/sh\nexec sleep 30\n"), 0700); err != nil {
					t.Fatal(err)
				}
				opts := ingressOptions{Host: host, Port: port, Token: "synthetic-token", Hostname: "hooks.example.test", Binary: binary}
				const recorderURL = "http://127.0.0.1:18444"
				var mu sync.Mutex
				registration := "https://hooks.example.test"
				control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.URL.Path == "/veris/client/probe":
						target := app.URL
						if recorderRoute {
							target = recorderURL
						}
						res, err := http.Get(target + "/")
						if err == nil {
							res.Body.Close()
						}
						// Even an answering unrelated app is insufficient routing evidence.
						io.WriteString(w, `{"probe_state":"answered"}`)
					case r.Method == http.MethodPatch:
						var body struct {
							Data struct {
								Client []struct {
									URL *string `json:"default_base_url"`
								} `json:"client"`
							} `json:"data"`
						}
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Error(err)
							w.WriteHeader(400)
							return
						}
						mu.Lock()
						registration = ""
						if body.Data.Client[0].URL != nil {
							registration = *body.Data.Client[0].URL
						}
						mu.Unlock()
						io.WriteString(w, `{}`)
					default:
						mu.Lock()
						url := registration
						mu.Unlock()
						json.NewEncoder(w).Encode(map[string]any{"rows": []any{map[string]any{"default_base_url": url}}})
					}
				}))
				defer control.Close()
				cfg := &config.Config{Services: []config.Service{{Name: "stripe", Upstream: control.URL}}}
				log := slog.New(slog.DiscardHandler)
				var pending *pendingIngress
				var err error
				if pendingMode {
					pending, err = openIngress(context.Background(), log, opts)
					if err != nil {
						t.Fatal(err)
					}
					defer pending.CloseOnFailure()
					// Prior startup traffic cannot stand in for a fresh readiness probe.
					if res, e := http.Get(recorderURL + "/"); e == nil {
						res.Body.Close()
					}
				}
				budget := 300 * time.Millisecond
				if recorderRoute {
					budget = 5 * time.Second
				}
				ctx, cancel := context.WithTimeout(context.Background(), budget)
				defer cancel()
				in, err := startIngress(ctx, log, opts, cfg, &proxy.Running{}, pending)
				if !recorderRoute {
					if in != nil {
						defer in.Stop(context.Background())
					}
					if err == nil || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), recorderURL) {
						t.Fatalf("bypass should fail with remote configuration remedy, got %v", err)
					}
					mu.Lock()
					remaining := registration
					mu.Unlock()
					if remaining != "" {
						t.Fatalf("failed run retained registration %q", remaining)
					}
					conn, e := net.DialTimeout("tcp", "127.0.0.1:18444", 100*time.Millisecond)
					if e == nil {
						conn.Close()
						t.Fatal("failed run left recorder listening")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				defer in.Stop(context.Background())
				if got := in.Inbound.Receipt().Total; got != 0 {
					t.Fatalf("startup probes counted as application callbacks: %d", got)
				}
				res, err := http.Post(recorderURL+"/webhook", "application/json", strings.NewReader(`{}`))
				if err != nil {
					t.Fatal(err)
				}
				res.Body.Close()
				receipt := in.Inbound.Receipt()
				if receipt.Total != 1 || receipt.Delivered != 1 || receipt.DeliveredByPath["/webhook"] != 1 {
					t.Fatalf("real callback missing: %+v", receipt)
				}
			})
		}
	}
}

func TestNamedRecorderCannotForwardToItself(t *testing.T) {
	_, err := listenForCallbacks(ingressOptions{Token: "synthetic-token", Host: "127.0.0.1", Port: 18444})
	if err == nil {
		t.Fatal("self-forwarding recorder must be refused")
	}
}

func TestQuickTunnelKeepsEphemeralRecorder(t *testing.T) {
	ln, err := listenForCallbacks(ingressOptions{Host: "127.0.0.1", Port: 3000})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if ln.Addr().String() == namedCallbackAddress {
		t.Fatal("quick tunnel took the named tunnel port")
	}
}

func TestNamedRecorderWaitsForRouteWithoutRequiringApp(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	inbound, err := proxy.NewIngress("127.0.0.1", port)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewServer(inbound.Handler())
	defer recorder.Close()
	probes := 0
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes++
		// The remote route becomes effective after the first probe.
		if probes > 1 {
			res, err := http.Get(recorder.URL)
			if err != nil {
				t.Error(err)
			} else {
				res.Body.Close()
			}
		}
		io.WriteString(w, `{"probe_state":"answered"}`)
	}))
	defer control.Close()
	in := &ingress{Inbound: inbound, log: slog.New(slog.DiscardHandler)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := in.waitForNamedRecorder(ctx, callback.New(control.URL, callback.Options{})); err != nil {
		t.Fatal(err)
	}
	receipt := inbound.Receipt()
	if receipt.Total != 1 || receipt.Failed != 1 || receipt.Delivered != 0 {
		t.Fatalf("recorder should be ready while app is absent: %+v", receipt)
	}
}
