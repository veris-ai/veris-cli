package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// UploadFolder streams the body as given, once, with the headers the route
// reads; a plane without the route answers FastAPI's 404, which surfaces
// as *Error{404, "Not Found"} rather than being retried.
func TestUploadFolderStreamsOnceAndSurfacesAMissingRoute(t *testing.T) {
	var hits atomic.Int32
	var got struct {
		method, path, query, contentType, key string
		length                                int64
		body                                  string
	}
	status := 201
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		raw, _ := io.ReadAll(r.Body)
		got.method, got.path, got.query = r.Method, r.URL.Path, r.URL.RawQuery
		got.contentType, got.key, got.length, got.body = r.Header.Get("Content-Type"), r.Header.Get("X-API-Key"), r.ContentLength, string(raw)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == 201 {
			_, _ = io.WriteString(w, `{"id":"up_1","bytes":9,"sha256":"abc"}`)
		} else {
			_, _ = io.WriteString(w, `{"detail":"Not Found"}`)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "vsk_test")

	out, err := c.UploadFolder(context.Background(), "env_1", "app", strings.NewReader("tar bytes"), 9)
	if err != nil {
		t.Fatal(err)
	}
	if out.ID != "up_1" || out.Bytes != 9 || out.SHA256 != "abc" {
		t.Errorf("got %+v", out)
	}
	if got.method != "POST" || got.path != "/v1/environments/env_1/uploads" || got.query != "name=app" {
		t.Errorf("sent %s %s?%s", got.method, got.path, got.query)
	}
	if got.contentType != "application/gzip" || got.key != "vsk_test" || got.length != 9 || got.body != "tar bytes" {
		t.Errorf("headers/body: %+v", got)
	}

	status = 404
	hits.Store(0)
	_, err = c.UploadFolder(context.Background(), "env_1", "", strings.NewReader("tar bytes"), 9)
	var ae *Error
	if !errors.As(err, &ae) || ae.Status != 404 || ae.Detail != "Not Found" {
		t.Fatalf("err = %v, want *Error{404, Not Found}", err)
	}
	if hits.Load() != 1 {
		t.Errorf("a POST was sent %d times; uploads are never retried", hits.Load())
	}
	if got.query != "" {
		t.Errorf("an empty name still sent ?%s", got.query)
	}
}
