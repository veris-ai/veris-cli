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
	"strings"
	"sync/atomic"
	"testing"

	"github.com/veris-ai/veris-cli/internal/twin"
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

// A /c/ control URL takes the Veris key on the upload too: the twin client's
// HTTP carries it, and a refused key says what to do rather than "HTTP 401".
func TestUploadCarriesTheVerisKeyAndNamesARefusal(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a"), []byte("body"), 0600)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if r.Header.Get("X-API-Key") != "vsk_good" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"detail":"invalid or missing API key"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"files":[]}`)
	}))
	defer server.Close()
	control := server.URL + "/c/sbx_1/box"
	o := Options{Source: root, ControlURL: control, Owner: "owner", Prefix: "Corpus", BatchBytes: 128, Checkpoint: filepath.Join(t.TempDir(), "checkpoint.json")}

	if _, err := Run(context.Background(), twin.NewWithKey(control, "vsk_good").HTTP, o, nil); err != nil {
		t.Fatalf("import with the key: %v", err)
	}
	o.Checkpoint = filepath.Join(t.TempDir(), "checkpoint.json")
	_, err := Run(context.Background(), twin.NewWithKey(control, "vsk_revoked").HTTP, o, nil)
	if err == nil || !strings.Contains(err.Error(), "control plane rejected the Veris credential; run veris login") {
		t.Fatalf("err = %v, want the credential named", err)
	}
}

// A checkpoint written while the sandbox's control_url was its /s/ data
// mount resumes on the /c/ control proxy that replaced it, without
// replaying what the twin already acknowledged -- and names the new target
// from then on. Another twin, sandbox or host is still another target.
func TestResumeFollowsTheControlURLFromTheDataMountToTheProxy(t *testing.T) {
	root := t.TempDir()
	check := filepath.Join(t.TempDir(), "checkpoint.json")
	os.WriteFile(filepath.Join(root, "a.pdf"), []byte("first"), 0600)
	os.WriteFile(filepath.Join(root, "b.pdf"), []byte("second"), 0600)
	var calls atomic.Int64
	var refuse atomic.Bool
	refuse.Store(true)
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		paths = append(paths, r.URL.Path)
		if calls.Load() == 2 && refuse.Load() {
			w.WriteHeader(422)
			return
		}
		io.WriteString(w, `{"created":1,"updated":0}`)
	}))
	defer server.Close()
	o := Options{Source: root, ControlURL: server.URL + "/s/sb_1/drive", Owner: "owner", Prefix: "Corpus", BatchBytes: 6, Checkpoint: check}
	if r, err := Run(context.Background(), server.Client(), o, nil); err == nil || r.Completed != 1 {
		t.Fatalf("expected a partial import: %+v %v", r, err)
	}
	refuse.Store(false)
	o.Resume = true
	for _, other := range []string{server.URL + "/c/sb_2/drive", server.URL + "/c/sb_1/sheets", "http://elsewhere.test/c/sb_1/drive"} {
		moved := o
		moved.ControlURL = other
		if _, err := Run(context.Background(), server.Client(), moved, nil); err == nil {
			t.Errorf("resumed against %s", other)
		}
	}
	o.ControlURL = server.URL + "/c/sb_1/drive"
	r, err := Run(context.Background(), server.Client(), o, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Completed != 2 || calls.Load() != 3 || paths[2] != "/c/sb_1/drive/veris/files" {
		t.Fatalf("resume replayed or went elsewhere: %+v, calls %d, paths %v", r, calls.Load(), paths)
	}
	saved, _ := os.ReadFile(check)
	if !strings.Contains(string(saved), "/c/sb_1/drive") {
		t.Errorf("checkpoint still names the old target: %s", saved)
	}
}
