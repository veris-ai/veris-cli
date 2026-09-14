package fileimport

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
)

func TestBatchesResumeWithoutReplayingAcknowledgedFiles(t *testing.T) {
	root := t.TempDir()
	check := filepath.Join(root, "checkpoint.json")
	os.WriteFile(filepath.Join(root, "a.pdf"), []byte("first"), 0600)
	os.Mkdir(filepath.Join(root, "nested"), 0700)
	os.WriteFile(filepath.Join(root, "nested/b.pdf"), []byte("second"), 0600)
	received := map[string]string{}
	var calls atomic.Int64
	var refuse atomic.Bool
	refuse.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("owner") != "owner" || r.URL.Query().Get("prefix") != "Corpus" || r.URL.Query().Get("mode") != "merge" {
			t.Error("destination not preserved")
		}
		if r.Header.Get("X-API-Key") != "" {
			t.Error("control-plane key leaked to capability route")
		}
		if calls.Load() == 2 && refuse.Load() {
			w.WriteHeader(422)
			return
		}
		b, _ := io.ReadAll(r.Body)
		z, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		for _, f := range z.File {
			body, _ := f.Open()
			data, _ := io.ReadAll(body)
			body.Close()
			if _, exists := received[f.Name]; exists {
				t.Error("replayed acknowledged file")
			}
			received[f.Name] = string(data)
		}
		io.WriteString(w, `{"created":1,"updated":0}`)
	}))
	defer server.Close()
	o := Options{Source: root, ControlURL: server.URL, Owner: "owner", Prefix: "Corpus", BatchBytes: 6, Checkpoint: check}
	r, err := Run(context.Background(), server.Client(), o, nil)
	if err == nil || r.Completed != 1 || len(r.Pending) != 0 {
		t.Fatalf("expected safe partial import: %+v %v", r, err)
	}
	refuse.Store(false)
	o.Resume = true
	r, err = Run(context.Background(), server.Client(), o, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Completed != 2 || r.Bytes != 11 || calls.Load() != 3 || !reflect.DeepEqual(received, map[string]string{"a.pdf": "first", "nested/b.pdf": "second"}) {
		t.Fatalf("incorrect result %+v / %v / %d", r, received, calls.Load())
	}
	_, err = Run(context.Background(), server.Client(), o, nil)
	if err != nil || calls.Load() != 3 {
		t.Fatal("completed checkpoint replayed", err)
	}
	os.WriteFile(filepath.Join(root, "a.pdf"), []byte("changed"), 0600)
	if _, err = Run(context.Background(), server.Client(), o, nil); err == nil || calls.Load() != 3 {
		t.Fatal("changed source resumed")
	}
}

func TestUnknownCommitRefusesAutomaticReplay(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a"), []byte("body"), 0600)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.Copy(io.Discard, r.Body)
		connection, _, _ := w.(http.Hijacker).Hijack()
		connection.Close()
	}))
	defer server.Close()
	o := Options{Source: root, ControlURL: server.URL, Owner: "owner", Prefix: "Corpus", BatchBytes: 128, Checkpoint: filepath.Join(t.TempDir(), "checkpoint.json")}
	r, err := Run(context.Background(), server.Client(), o, nil)
	if err == nil || len(r.Pending) != 1 {
		t.Fatal("uncertain POST not retained")
	}
	o.Resume = true
	if _, err = Run(context.Background(), server.Client(), o, nil); err == nil || calls.Load() != 1 {
		t.Fatal("uncertain POST replayed")
	}
}

func TestOversizedBatchMemberStreamsRawAndCheckpointContainsHashes(t *testing.T) {
	root := t.TempDir()
	payload := bytes.Repeat([]byte("data"), 1024)
	os.WriteFile(filepath.Join(root, "large.pdf"), payload, 0600)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("path") != "large.pdf" || r.Header.Get("Content-Type") != "application/octet-stream" {
			t.Error("large file not raw")
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Equal(body, payload) || r.ContentLength != int64(len(payload)) {
			t.Error("body changed")
		}
		io.WriteString(w, `{"created":1}`)
	}))
	defer server.Close()
	o := Options{Source: root, ControlURL: server.URL, Owner: "owner", Prefix: "Corpus", BatchBytes: 128, Checkpoint: filepath.Join(t.TempDir(), "checkpoint.json")}
	r, err := Run(context.Background(), server.Client(), o, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(o.Checkpoint)
	var saved Receipt
	if err = json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Completed != 1 || saved.Files[0].SHA256 == "" || saved.Bytes != int64(len(payload)) || r.Completed != 1 {
		t.Fatal("missing checkpoint evidence")
	}
}
