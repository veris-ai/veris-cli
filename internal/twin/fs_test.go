package twin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The filesystem routes pin their wire shapes: the token POST carries ttl_s,
// the layout decodes, diff passes folder and format through and returns the
// body verbatim with the truncation header read, export streams the bytes.
func TestFilesystemRoutesSendWhatTheEngineExpects(t *testing.T) {
	ctx := context.Background()

	t.Run("token", func(t *testing.T) {
		c, got := fakeTwin(t, 200, `{"user":"u1","password":"p1","expires_at":"2026-10-05T13:00:00Z"}`)
		tok, err := c.FSToken(ctx, 30*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if got.Method != "POST" || got.Path != "/s/sbx_1/stripe/veris/fs/token" || !sameJSON(got.Body, `{"ttl_s":1800}`) {
			t.Errorf("sent %s %s %s", got.Method, got.Path, got.Body)
		}
		if tok.User != "u1" || tok.Password != "p1" {
			t.Errorf("got %+v", tok)
		}
		if at, ok := tok.Expiry(); !ok || !at.Equal(time.Date(2026, 10, 5, 13, 0, 0, 0, time.UTC)) {
			t.Errorf("expiry = %v %v", at, ok)
		}
		// No ttl sends an empty body, so the engine's default applies.
		if _, err := c.FSToken(ctx, 0); err != nil || !sameJSON(got.Body, `{}`) {
			t.Errorf("ttl 0 sent %s (%v)", got.Body, err)
		}
	})

	t.Run("token without credentials is an error", func(t *testing.T) {
		c, _ := fakeTwin(t, 200, `{"user":"","password":""}`)
		if _, err := c.FSToken(ctx, 0); err == nil {
			t.Error("an answer without credentials was accepted")
		}
	})

	t.Run("layout", func(t *testing.T) {
		c, got := fakeTwin(t, 200, `{"primary":"app","root":"app","folders":[
			{"name":"app","root":true,"source":{"kind":"git","url":"https://github.com/acme/app","ref":"main","sha":"abc"},"bytes":10,"files":2,"ready":true,"modified":false},
			{"name":"data","root":false,"source":{"kind":"upload","id":"up_1"},"bytes":0,"files":0,"ready":false,"modified":true}]}`)
		l, err := c.FSLayout(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got.Method != "GET" || got.Path != "/s/sbx_1/stripe/veris/fs/layout" {
			t.Errorf("sent %s %s", got.Method, got.Path)
		}
		if l.Primary != "app" || l.Root == nil || *l.Root != "app" || len(l.Folders) != 2 {
			t.Fatalf("got %+v", l)
		}
		if f := l.Folders[0]; !f.Root || f.Source.Kind != "git" || f.Source.SHA != "abc" || f.Source.Where() != "https://github.com/acme/app@main" {
			t.Errorf("folder 0 = %+v", f)
		}
		if f := l.Folders[1]; f.Source.Where() != "up_1" || !f.Modified || f.Ready {
			t.Errorf("folder 1 = %+v", f)
		}
	})

	t.Run("diff", func(t *testing.T) {
		c, got := fakeTwin(t, 200, `{"folder":"app","files_changed":1}`)
		body, truncated, err := c.FSDiff(ctx, "app", FSDiffStatus)
		if err != nil {
			t.Fatal(err)
		}
		if got.Method != "GET" || got.Path != "/s/sbx_1/stripe/veris/fs/diff" || got.Query != "folder=app&format=status" {
			t.Errorf("sent %s %s?%s", got.Method, got.Path, got.Query)
		}
		if string(body) != `{"folder":"app","files_changed":1}` || truncated {
			t.Errorf("body %q truncated %v", body, truncated)
		}
		if _, _, err := c.FSDiff(ctx, "app", "colour"); err == nil {
			t.Error("an unknown format was sent")
		}
		if _, _, err := c.FSDiff(ctx, "", FSDiffStatus); err == nil {
			t.Error("an empty folder was sent")
		}
	})

	t.Run("a truncated patch says so", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/x-diff")
			w.Header().Set("X-Veris-Truncated", "true")
			_, _ = io.WriteString(w, "--- a\n+++ b\n")
		}))
		defer srv.Close()
		body, truncated, err := New(srv.URL+"/s/sbx_1/filesystem").FSDiff(ctx, "app", FSDiffPatch)
		if err != nil || string(body) != "--- a\n+++ b\n" || !truncated {
			t.Errorf("body %q truncated %v err %v", body, truncated, err)
		}
	})

	t.Run("export streams with the key on the transport", func(t *testing.T) {
		var key, query string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key, query = r.Header.Get("X-API-Key"), r.URL.RawQuery
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write([]byte("gzip tar bytes"))
		}))
		defer srv.Close()
		c := NewWithKey(srv.URL+"/c/sbx_1/filesystem", "vsk_k")
		rc, err := c.FSExport(ctx, "app")
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		raw, _ := io.ReadAll(rc)
		if string(raw) != "gzip tar bytes" || key != "vsk_k" || query != "folder=app" {
			t.Errorf("got %q key %q query %q", raw, key, query)
		}
	})
}

// Every filesystem route is optional: FastAPI's 404 on each is
// ErrNotSupported, and a 401 through the /c/ proxy is ErrUnauthorized.
func TestFilesystemRoutesClassifyRefusals(t *testing.T) {
	ctx := context.Background()
	calls := map[string]func(c *Client) error{
		"token":  func(c *Client) error { _, err := c.FSToken(ctx, 0); return err },
		"layout": func(c *Client) error { _, err := c.FSLayout(ctx); return err },
		"diff":   func(c *Client) error { _, _, err := c.FSDiff(ctx, "app", FSDiffStatus); return err },
		"export": func(c *Client) error { _, err := c.FSExport(ctx, "app"); return err },
	}
	for name, call := range calls {
		t.Run(name+" 404", func(t *testing.T) {
			c, _ := fakeTwin(t, 404, `{"detail":"Not Found"}`)
			if err := call(c); !errors.Is(err, ErrNotSupported) {
				t.Errorf("err = %v, want ErrNotSupported", err)
			}
		})
		t.Run(name+" 401 through /c/", func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(401)
				_, _ = io.WriteString(w, `{"detail":"invalid or missing API key"}`)
			}))
			defer srv.Close()
			err := call(NewWithKey(srv.URL+"/c/sbx_1/filesystem", "vsk_bad"))
			if !errors.Is(err, ErrUnauthorized) || !strings.Contains(err.Error(), "veris login") {
				t.Errorf("err = %v, want ErrUnauthorized", err)
			}
		})
	}
}
