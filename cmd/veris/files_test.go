package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"github.com/veris-ai/veris-cli/internal/api"
	"github.com/veris-ai/veris-cli/internal/fileimport"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestSandboxFilesImportAndResume(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "one.pdf"), []byte("PDF payload"), 0600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "POST" || r.URL.Path != "/veris/files" || r.URL.Query().Get("owner") != "owner" || r.URL.Query().Get("prefix") != "Corpus" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		z, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
		if err != nil || len(z.File) != 1 {
			t.Error("invalid ZIP", err)
			return
		}
		f, err := z.File[0].Open()
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		data, _ := io.ReadAll(f)
		if z.File[0].Name != "one.pdf" || string(data) != "PDF payload" {
			t.Error("wrong staged bytes")
		}
		io.WriteString(w, `{"created":1}`)
	}))
	defer server.Close()
	plane := newSandboxPlane(t)
	dataBench(t, plane, []api.ServiceInfo{{Name: "google-drive", Status: "ready", URL: server.URL, ControlURL: server.URL}})
	check := filepath.Join(t.TempDir(), "checkpoint.json")
	args := []string{"sandbox", "files", "import", "google-drive", root, "--owner", "owner", "--prefix", "Corpus", "--checkpoint", check, "--json"}
	for attempt := 0; attempt < 2; attempt++ {
		code, out, errout := runSandboxCLI(t, args...)
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errout)
		}
		var result fileimport.Receipt
		if err := json.Unmarshal([]byte(out), &result); err != nil || result.Completed != 1 || result.Bytes != 11 || len(result.Files) != 1 || result.Files[0].SHA256 == "" {
			t.Fatalf("invalid receipt %s: %v", out, err)
		}
		args = append(args, "--resume")
	}
	if calls.Load() != 1 {
		t.Fatal("acknowledged upload replayed")
	}
}
