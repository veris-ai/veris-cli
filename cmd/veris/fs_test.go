package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/veris-ai/veris-cli/internal/api"
	"github.com/veris-ai/veris-cli/internal/cfg"
	"github.com/veris-ai/veris-cli/internal/twin"
)

// fsEngine is a fake filesystem member: the control routes under
// /c/<sandbox>/filesystem/veris/fs/* behind the Veris key, health on both
// the control and data mounts for up's probes, and a WebDAV root under
// /s/<sandbox>/filesystem/ that takes MKCOL and PUT with the minted
// credentials. Everything scripted is behind the mutex.
type fsEngine struct {
	srv *httptest.Server
	mu  sync.Mutex

	old      bool // every /veris/fs/* answers FastAPI's 404
	layout   twin.FSLayout
	export   map[string]map[string]string // folder → tar entries; "-> x" is a symlink
	diffBody string
	diffCut  bool
	// readyAfter, when > 0, is the layout read (1-based) on which every
	// folder turns ready, standing in for a pull that finishes while a verb
	// waits; layoutReads counts them.
	readyAfter  int
	layoutReads int

	// exportStall, when set, sends the first half of the export and then
	// holds the stream open (until the client leaves, or exportStallMax)
	// -- an engine that stops mid-transfer.
	exportStall bool
	// diffStall holds a diff request open until the client leaves, or
	// exportStallMax.
	diffStall bool

	tokens    []int // ttl_s of every mint; -1 when absent
	diffQuery string
	dav       []davCall
	// remote is what the WebDAV root already holds, path (under the root,
	// no slashes at either end) → whether it is a collection; PROPFIND
	// lists it and DELETE removes from it.
	remote map[string]bool
}

type davCall struct {
	Method, Path, Auth, Body string
}

const (
	fsUser = "u_1"
	fsPass = "p_secret_1"
)

// exportStallMax is how long a stalled export holds its stream open when
// the client never gives up on it.
const exportStallMax = 5 * time.Second

func newFSEngine(t *testing.T) *fsEngine {
	t.Helper()
	f := &fsEngine{
		layout: twin.FSLayout{Primary: "app", Root: strp("app"), Folders: []twin.FSFolder{
			{Name: "app", Root: true, Source: twin.FSSource{Kind: "git", URL: "https://github.com/acme/app", Ref: "main", SHA: "abc123"}, Bytes: 10, Files: 2, Ready: true},
		}},
		export: map[string]map[string]string{
			"app": {"README.md": "hi\n", "src/": "", "src/main.py": "print(1)\n", "link.md": "-> README.md"},
		},
		diffBody: `{"folder":"app","baseline_sha":"abc1234567","files_changed":2,"insertions":12,"deletions":3,"files":[{"path":"src/main.py","status":"M","insertions":10,"deletions":3},{"path":"notes.md","status":"A","insertions":2,"deletions":0}]}`,
	}
	mux := http.NewServeMux()
	health := func(w http.ResponseWriter, r *http.Request) {
		sbJSON(w, 200, map[string]any{"status": "ok", "service": "filesystem"})
	}
	mux.HandleFunc("/c/"+sbID+"/filesystem/veris/health", health)
	mux.HandleFunc("/s/"+sbID+"/filesystem/veris/health", health)
	control := func(handle func(w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-API-Key") != sbTestKey {
				sbJSON(w, 401, map[string]string{"detail": "invalid or missing API key"})
				return
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.old {
				sbJSON(w, 404, map[string]string{"detail": "Not Found"})
				return
			}
			handle(w, r)
		}
	}
	mux.HandleFunc("POST /c/"+sbID+"/filesystem/veris/fs/token", control(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			TTL *int `json:"ttl_s"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		ttl := -1
		if body.TTL != nil {
			ttl = *body.TTL
		}
		f.tokens = append(f.tokens, ttl)
		sbJSON(w, 200, map[string]any{"user": fsUser, "password": fsPass, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	}))
	mux.HandleFunc("GET /c/"+sbID+"/filesystem/veris/fs/layout", control(func(w http.ResponseWriter, r *http.Request) {
		f.layoutReads++
		if f.readyAfter > 0 && f.layoutReads >= f.readyAfter {
			for i := range f.layout.Folders {
				f.layout.Folders[i].Ready = true
			}
		}
		sbJSON(w, 200, f.layout)
	}))
	mux.HandleFunc("GET /c/"+sbID+"/filesystem/veris/fs/diff", control(func(w http.ResponseWriter, r *http.Request) {
		f.diffQuery = r.URL.RawQuery
		if f.diffStall {
			f.mu.Unlock()
			defer f.mu.Lock()
			select {
			case <-r.Context().Done():
			case <-time.After(exportStallMax):
			}
			return
		}
		if r.URL.Query().Get("format") == "patch" {
			w.Header().Set("Content-Type", "text/x-diff")
			if f.diffCut {
				w.Header().Set("X-Veris-Truncated", "true")
			}
			_, _ = io.WriteString(w, "--- a/src/main.py\n+++ b/src/main.py\n@@ -1 +1 @@\n-print(0)\n+print(1)\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, f.diffBody)
	}))
	mux.HandleFunc("GET /c/"+sbID+"/filesystem/veris/fs/export", control(func(w http.ResponseWriter, r *http.Request) {
		entries, ok := f.export[r.URL.Query().Get("folder")]
		if !ok {
			sbJSON(w, 404, map[string]string{"detail": "no folder " + r.URL.Query().Get("folder")})
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		body := gzipTar(t, entries)
		if !f.exportStall {
			_, _ = w.Write(body)
			return
		}
		_, _ = w.Write(body[:len(body)/2])
		w.(http.Flusher).Flush()
		// The other routes stay answerable while this one stalls.
		f.mu.Unlock()
		defer f.mu.Lock()
		select {
		case <-r.Context().Done():
		case <-time.After(exportStallMax):
		}
	}))
	mux.HandleFunc("/s/"+sbID+"/filesystem/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		user, pass, ok := r.BasicAuth()
		f.mu.Lock()
		auth := ""
		if ok {
			auth = user + ":" + pass
		}
		f.dav = append(f.dav, davCall{Method: r.Method, Path: strings.TrimPrefix(r.URL.Path, "/s/"+sbID+"/filesystem/"), Auth: auth, Body: string(body)})
		f.mu.Unlock()
		if !ok || user != fsUser || pass != fsPass {
			w.WriteHeader(401)
			return
		}
		rel := strings.Trim(strings.TrimPrefix(r.URL.Path, "/s/"+sbID+"/filesystem/"), "/")
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case "MKCOL", http.MethodPut:
			w.WriteHeader(http.StatusCreated)
		case "PROPFIND":
			isDir, ok := f.remote[rel]
			if !ok || !isDir || r.Header.Get("Depth") != "1" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			var b strings.Builder
			b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><D:multistatus xmlns:D="DAV:">`)
			entry := func(p string, dir bool) {
				href := "/s/" + sbID + "/filesystem/" + escapePath(p)
				kind := "<D:resourcetype/>"
				if dir {
					href += "/"
					kind = "<D:resourcetype><D:collection/></D:resourcetype>"
				}
				b.WriteString("<D:response><D:href>" + href + "</D:href><D:propstat><D:prop>" + kind + "</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>")
			}
			entry(rel, true)
			for p, dir := range f.remote {
				if path.Dir(p) == rel {
					entry(p, dir)
				}
			}
			b.WriteString("</D:multistatus>")
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = io.WriteString(w, b.String())
		case http.MethodDelete:
			if _, ok := f.remote[rel]; !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			delete(f.remote, rel)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// gzipTar builds a gzip tar from name → content, in sorted-insertion
// order of the map's keys as given by the caller's ranging; a trailing
// slash is a directory, "-> target" a symlink, "=> target" a hard link,
// "|fifo" a named pipe. Names are written as given,
// so a test can send a hostile one.
func gzipTar(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	// Hostile names first, so a refusal is proven to land before any byte.
	for i := range names {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	for _, name := range names {
		body := entries[name]
		hdr := &tar.Header{Name: name, Mode: 0o644, Typeflag: tar.TypeReg, Size: int64(len(body)), Format: tar.FormatPAX}
		switch {
		case strings.HasSuffix(name, "/"):
			hdr.Typeflag, hdr.Mode, hdr.Size = tar.TypeDir, 0o755, 0
		case strings.HasPrefix(body, "-> "):
			hdr.Typeflag, hdr.Linkname, hdr.Size = tar.TypeSymlink, strings.TrimPrefix(body, "-> "), 0
		case strings.HasPrefix(body, "=> "):
			hdr.Typeflag, hdr.Linkname, hdr.Size = tar.TypeLink, strings.TrimPrefix(body, "=> "), 0
		case body == "|fifo":
			hdr.Typeflag, hdr.Size = tar.TypeFifo, 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func (f *fsEngine) script(fn func(f *fsEngine)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fsEngine) minted() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.tokens...)
}

func (f *fsEngine) davCalls() []davCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]davCall(nil), f.dav...)
}

func (f *fsEngine) lastDiffQuery() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.diffQuery
}

// service is the filesystem member as the control plane lists it: the
// WebDAV root with its trailing slash, the /c/ control URL, no env hint.
func (f *fsEngine) service() api.ServiceInfo {
	return api.ServiceInfo{Name: filesystemService, Status: "ready",
		URL: f.srv.URL + "/s/" + sbID + "/filesystem/", ControlURL: f.srv.URL + "/c/" + sbID + "/filesystem",
		ControlAuth: strp("api_key")}
}

// fsBench is a logged-in bench whose folder points at a ready sandbox with
// stripe, postgres and the filesystem.
func fsBench(t *testing.T) (*fsEngine, *bench) {
	t.Helper()
	plane := newSandboxPlane(t)
	twins := newDataTwins(t)
	engine := newFSEngine(t)
	b, _ := dataBench(t, plane, append(twins.services(), engine.service()))
	return engine, b
}

// A folder whose pull has not finished (layout ready:false) is waited for
// under a spinner by the verbs that read or write it, and refused with its
// name once the wait runs out; connect, which only mints, names it.
func TestFsVerbsWaitForAFolderStillPulling(t *testing.T) {
	engine, _ := fsBench(t)
	fsReadyPoll, fsReadyTimeout = 5*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { fsReadyPoll, fsReadyTimeout = 2*time.Second, 5*time.Minute })
	notReady := func(readyAfter int) {
		engine.script(func(f *fsEngine) {
			f.layout.Folders[0].Ready = false
			f.readyAfter, f.layoutReads = readyAfter, 0
		})
	}

	notReady(3)
	dest := filepath.Join(t.TempDir(), "app")
	code, _, stderr := runSandboxCLI(t, "sandbox", "fs", "pull", "app", "--dest", dest)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	sbInOrder(t, stderr, "Waiting for app to finish pulling (https://github.com/acme/app@main)\n", "✓ Pulled app: 2 files")
	if _, err := os.Stat(filepath.Join(dest, "src", "main.py")); err != nil {
		t.Errorf("the pull did not land after the wait: %v", err)
	}

	// Never ready: the wait runs out, nothing is exported, and the folder
	// and its source are named.
	notReady(0)
	code, _, stderr = runSandboxCLI(t, "sandbox", "fs", "pull", "app", "--dest", filepath.Join(t.TempDir(), "app"))
	if code != 1 || !strings.Contains(stderr, "✗ Folder app of sandbox "+sbID+" is still being pulled from https://github.com/acme/app@main after 200ms; try again once it is ready") {
		t.Errorf("exit %d:\n%s", code, stderr)
	}
	if strings.Contains(stderr, "Pulling app") {
		t.Errorf("an export was started on a folder still pulling:\n%s", stderr)
	}

	// push and diff hold the same line; nothing reaches the folder.
	notReady(0)
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr = runSandboxCLI(t, "sandbox", "fs", "push", "app", "--src", src); code != 1 || !strings.Contains(stderr, "is still being pulled") || len(engine.davCalls()) != 0 || len(engine.minted()) != 0 {
		t.Errorf("push: exit %d, dav %d, minted %d:\n%s", code, len(engine.davCalls()), len(engine.minted()), stderr)
	}
	if code, _, stderr = runSandboxCLI(t, "sandbox", "fs", "diff", "app"); code != 1 || !strings.Contains(stderr, "is still being pulled") || engine.lastDiffQuery() != "" {
		t.Errorf("diff: exit %d, query %q:\n%s", code, engine.lastDiffQuery(), stderr)
	}

	// attach waits for every folder and records the sha the finished
	// layout carries.
	notReady(2)
	dir := t.TempDir()
	code, stdout, stderr := runSandboxCLI(t, "sandbox", "fs", "attach", dir)
	if code != 0 || !strings.Contains(stderr, "Waiting for app to finish pulling") || !strings.Contains(stdout, `"sha":"abc123"`) {
		t.Errorf("attach: exit %d\n%s\n%s", code, stdout, stderr)
	}

	// connect mints regardless, and says which folders are still filling.
	notReady(0)
	code, stdout, stderr = runSandboxCLI(t, "sandbox", "fs", "connect")
	if code != 0 || !strings.Contains(stdout, "export VERIS_FS_PASSWORD=") || !strings.Contains(stderr, "! Still pulling: app; diff, pull, push and mount wait for them") {
		t.Errorf("connect: exit %d\n%s\n%s", code, stdout, stderr)
	}
}

func TestFsConnectMintsAndPrintsExports(t *testing.T) {
	engine, _ := fsBench(t)
	code, stdout, stderr := runSandboxCLI(t, "sandbox", "fs", "connect", "--ttl", "30m")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	want := "export VERIS_FS_URL='" + engine.srv.URL + "/s/" + sbID + "/filesystem/'\n" +
		"export VERIS_FS_USER='u_1'\n" +
		"export VERIS_FS_PASSWORD='p_secret_1'\n"
	if stdout != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", stdout, want)
	}
	if strings.Contains(stderr, fsPass) {
		t.Errorf("the password must not reach stderr:\n%s", stderr)
	}
	sbInOrder(t, stderr, "Filesystem of "+sbID+": 1 folder (app) · expires ", "rclone lsd :webdav,url=$VERIS_FS_URL")
	if got := engine.minted(); len(got) != 1 || got[0] != 1800 {
		t.Errorf("minted ttl_s = %v, want [1800]", got)
	}

	code, stdout, stderr = runSandboxCLI(t, "sandbox", "fs", "connect", "--json")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	var body struct {
		URL       string   `json:"url"`
		User      string   `json:"user"`
		Password  string   `json:"password"`
		ExpiresAt string   `json:"expires_at"`
		Folders   []string `json:"folders"`
	}
	if err := json.Unmarshal([]byte(stdout), &body); err != nil || body.User != fsUser || body.Password != fsPass || body.URL == "" || body.ExpiresAt == "" || len(body.Folders) != 1 {
		t.Errorf("--json: %v\n%s", err, stdout)
	}
	if got := engine.minted(); len(got) != 2 || got[1] != 3600 {
		t.Errorf("the default ttl is an hour: %v", got)
	}

	if code, _, stderr := runSandboxCLI(t, "sandbox", "fs", "connect", "--ttl", "25h"); code != 1 || !strings.Contains(stderr, "more than the engine allows") {
		t.Errorf("exit %d:\n%s", code, stderr)
	}
}

func TestFsDiffDefaultsToTheOnlyFolderAndNamesThemWhenAmbiguous(t *testing.T) {
	engine, _ := fsBench(t)
	code, stdout, stderr := runSandboxCLI(t, "sandbox", "fs", "diff")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	if engine.lastDiffQuery() != "folder=app&format=status" {
		t.Errorf("asked %q", engine.lastDiffQuery())
	}
	sbInOrder(t, stderr, "sandbox fs diff of app, the sandbox's only folder")
	sbInOrder(t, stdout,
		"app · 2 files changed, +12 -3 (baseline abc12345…)\n",
		"M  src/main.py +10 -3\n",
		"A  notes.md    +2\n")

	code, stdout, stderr = runSandboxCLI(t, "sandbox", "fs", "diff", "app", "--format", "patch")
	if code != 0 || !strings.HasPrefix(stdout, "--- a/src/main.py\n+++ b/src/main.py\n") || strings.Contains(stderr, "cut") {
		t.Errorf("patch: exit %d\n%s\n%s", code, stdout, stderr)
	}
	engine.script(func(f *fsEngine) { f.diffCut = true })
	if code, _, stderr = runSandboxCLI(t, "sandbox", "fs", "diff", "app", "--format", "patch"); code != 0 || !strings.Contains(stderr, "! The patch was cut at the engine's cap") {
		t.Errorf("truncated patch: exit %d\n%s", code, stderr)
	}

	code, stdout, stderr = runSandboxCLI(t, "sandbox", "fs", "diff", "--json")
	if code != 0 || !strings.Contains(stdout, `"baseline_sha": "abc1234567"`) {
		t.Errorf("--json: exit %d\n%s\n%s", code, stdout, stderr)
	}

	engine.script(func(f *fsEngine) {
		f.layout.Folders = append(f.layout.Folders, twin.FSFolder{Name: "data", Source: twin.FSSource{Kind: "upload", ID: "up_1"}, Ready: true})
	})
	code, _, stderr = runSandboxCLI(t, "sandbox", "fs", "diff", "--id", sbID)
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, stderr)
	}
	sbInOrder(t, stderr, "✗ sandbox fs diff needs the folder's name. Sandbox "+sbID+" has 2:",
		"→ Next: veris sandbox fs diff app --id "+sbID, "→ Next: veris sandbox fs diff data --id "+sbID)

	if code, _, stderr = runSandboxCLI(t, "sandbox", "fs", "diff", "nope"); code != 1 || !strings.Contains(stderr, "No folder named 'nope' in sandbox "+sbID+" (have: app, data)") {
		t.Errorf("exit %d:\n%s", code, stderr)
	}
	if code, _, stderr = runSandboxCLI(t, "sandbox", "fs", "diff", "--format", "colour"); code != 1 || !strings.Contains(stderr, "--format must be status or patch") {
		t.Errorf("exit %d:\n%s", code, stderr)
	}
}

func TestFsPullUntarsAndRefusesTraversal(t *testing.T) {
	engine, b := fsBench(t)
	dest := filepath.Join(b.project, "out")
	code, _, stderr := runSandboxCLI(t, "sandbox", "fs", "pull", "--dest", dest)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	sbInOrder(t, stderr, "✓ Pulled app: 2 files, 12 B → "+dest, "→ veris sandbox fs push app --src "+dest)
	if got, _ := os.ReadFile(filepath.Join(dest, "src", "main.py")); string(got) != "print(1)\n" {
		t.Errorf("src/main.py = %q", got)
	}
	if target, err := os.Readlink(filepath.Join(dest, "link.md")); err != nil || target != "README.md" {
		t.Errorf("link.md -> %q, %v", target, err)
	}

	// A second pull into the same place needs --force.
	if code, _, stderr = runSandboxCLI(t, "sandbox", "fs", "pull", "--dest", dest); code != 1 || !strings.Contains(stderr, "is not empty") || !strings.Contains(stderr, "--force") {
		t.Errorf("exit %d:\n%s", code, stderr)
	}
	if code, _, stderr = runSandboxCLI(t, "sandbox", "fs", "pull", "--dest", dest, "--force"); code != 0 {
		t.Errorf("--force: exit %d:\n%s", code, stderr)
	}
	// The default destination is ./FOLDER.
	if code, _, stderr = runSandboxCLI(t, "sandbox", "fs", "pull", "app"); code != 0 {
		t.Errorf("exit %d:\n%s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(b.project, "app", "README.md")); err != nil {
		t.Errorf("./app not pulled: %v", err)
	}

	// With --force the destination may already hold symlinks; an entry is
	// never written through one. The outside file and directory stay as
	// they were.
	outside := filepath.Join(b.project, "outside")
	if err := os.MkdirAll(filepath.Join(outside, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "config"), []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(b.project, "linked")
	if err := os.MkdirAll(linked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "config"), filepath.Join(linked, "config")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "dir"), filepath.Join(linked, "sub")); err != nil {
		t.Fatal(err)
	}
	// An archive that names the directory replaces the symlinks sitting
	// where its entries go, so nothing is written through them.
	engine.script(func(f *fsEngine) {
		f.export["app"] = map[string]string{"config": "replaced\n", "sub/": "", "sub/x.txt": "inside\n"}
	})
	code, _, stderr = runSandboxCLI(t, "sandbox", "fs", "pull", "app", "--dest", linked, "--force")
	if code != 0 {
		t.Errorf("exit %d:\n%s", code, stderr)
	}
	if got, _ := os.ReadFile(filepath.Join(outside, "config")); string(got) != "keep\n" {
		t.Errorf("the file behind the symlink was written: %q", got)
	}
	if _, err := os.Stat(filepath.Join(outside, "dir", "x.txt")); err == nil {
		t.Error("x.txt was written through the symlinked parent")
	}
	for _, name := range []string{"config", "sub"} {
		if fi, err := os.Lstat(filepath.Join(linked, name)); err != nil || fi.Mode()&os.ModeSymlink != 0 {
			t.Errorf("%s: %v %v, want the symlink replaced", name, fi, err)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(linked, "sub", "x.txt")); string(got) != "inside\n" {
		t.Errorf("sub/x.txt = %q", got)
	}
	// One that reaches a file through a symlinked parent it never named is
	// refused, and the parent is left as it was.
	if err := os.Symlink(filepath.Join(outside, "dir"), filepath.Join(linked, "sub2")); err != nil {
		t.Fatal(err)
	}
	engine.script(func(f *fsEngine) { f.export["app"] = map[string]string{"sub2/x.txt": "escaped\n"} })
	code, _, stderr = runSandboxCLI(t, "sandbox", "fs", "pull", "app", "--dest", linked, "--force")
	if code != 1 || !strings.Contains(stderr, "a symlink in the destination leads outside it") {
		t.Errorf("exit %d:\n%s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(outside, "dir", "x.txt")); err == nil {
		t.Error("x.txt was written through the symlinked parent")
	}
	engine.script(func(f *fsEngine) {
		f.export["app"] = map[string]string{"README.md": "hi\n", "src/": "", "src/main.py": "print(1)\n", "link.md": "-> README.md"}
	})

	for name, entries := range map[string]map[string]string{
		"../x":             {"../x": "escaped\n", "ok.txt": "fine\n"},
		"/abs":             {"/abs": "escaped\n", "ok.txt": "fine\n"},
		"a/../../y":        {"a/../../y": "escaped\n", "ok.txt": "fine\n"},
		"a symlink out":    {"a/": "", "a/link": "-> ../../etc", "ok.txt": "fine\n"},
		"an absolute link": {"a/": "", "a/link": "-> /etc/passwd", "ok.txt": "fine\n"},
	} {
		t.Run(name, func(t *testing.T) {
			engine.script(func(f *fsEngine) { f.export["evil"] = entries })
			engine.script(func(f *fsEngine) {
				f.layout.Folders = []twin.FSFolder{{Name: "evil", Source: twin.FSSource{Kind: "upload", ID: "up_9"}, Ready: true}}
			})
			t.Cleanup(func() {
				engine.script(func(f *fsEngine) {
					f.layout.Folders = []twin.FSFolder{{Name: "app", Root: true, Source: twin.FSSource{Kind: "git", URL: "https://github.com/acme/app"}, Ready: true}}
				})
			})
			evil := filepath.Join(b.project, "evil-"+strings.NewReplacer("/", "_", " ", "_", ".", "_").Replace(name))
			code, _, stderr := runSandboxCLI(t, "sandbox", "fs", "pull", "evil", "--dest", evil)
			if code != 1 || !strings.Contains(stderr, "is refused") {
				t.Fatalf("exit %d:\n%s", code, stderr)
			}
			if _, err := os.Stat(filepath.Join(b.project, "x")); err == nil {
				t.Error("../x landed beside the destination")
			}
			if _, err := os.Stat(filepath.Join(evil, "ok.txt")); err == nil {
				t.Error("ok.txt was written although the archive was refused")
			}
		})
	}
}

// untar into "." -- `pull --dest .` -- lands nested entries: the
// destination is held to its absolute, resolved self, so a parent that
// resolves to "src" is inside a destination named ".".
func TestUntarIntoTheWorkingDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	archive := gzipTar(t, map[string]string{"README.md": "hi\n", "src/": "", "src/pkg/": "", "src/pkg/main.py": "print(1)\n", "deep/er/x.txt": "x\n"})
	sum, err := untar(bytes.NewReader(archive), ".")
	if err != nil {
		t.Fatalf("untar into .: %v", err)
	}
	if sum.Files != 3 {
		t.Errorf("files = %d, want 3", sum.Files)
	}
	for name, want := range map[string]string{"README.md": "hi\n", "src/pkg/main.py": "print(1)\n", "deep/er/x.txt": "x\n"} {
		if got, err := os.ReadFile(filepath.FromSlash(name)); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v", name, got, err)
		}
	}
}

// An entry under a destination symlink that leads outside is refused
// before anything of it is made: no directory appears on the far side of
// the link, at any depth.
func TestUntarMakesNothingThroughAnEscapingSymlink(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	dest := filepath.Join(base, "dest")
	for _, dir := range []string{outside, dest} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(dest, "link")); err != nil {
		t.Fatal(err)
	}
	for name, entries := range map[string]map[string]string{
		"file":      {"link/new/deeper/x.txt": "escaped\n"},
		"directory": {"link/new/deeper/": ""},
		"symlink":   {"link/new/deeper/l": "-> x"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := untar(bytes.NewReader(gzipTar(t, entries)), dest)
			if err == nil || !strings.Contains(err.Error(), "a symlink in the destination leads outside it") {
				t.Errorf("err = %v, want the escaping symlink named", err)
			}
			if got, _ := os.ReadDir(outside); len(got) != 0 {
				t.Errorf("written outside the destination: %v", got)
			}
			if fi, err := os.Lstat(filepath.Join(dest, "link")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
				t.Errorf("the destination's symlink was disturbed: %v %v", fi, err)
			}
		})
	}
}

// A link entry's target is held against where its parent really is, not
// where its name says: with --force the destination may hold a symlinked
// directory (inside it) whose far side sits higher than the name's depth,
// so a target that climbs out of the name's depth lexically stays inside
// while on disk it leaves. Nothing is made for the refused entry.
func TestUntarHoldsALinkToItsResolvedParent(t *testing.T) {
	base := t.TempDir()
	dest := filepath.Join(base, "dest")
	if err := os.MkdirAll(filepath.Join(dest, "d1", "d2"), 0o755); err != nil {
		t.Fatal(err)
	}
	// d1/d2/up is the destination itself: inside, so writing through it
	// is allowed, but it sits two levels above its name.
	if err := os.Symlink(filepath.Join("..", ".."), filepath.Join(dest, "d1", "d2", "up")); err != nil {
		t.Fatal(err)
	}
	_, err := untar(bytes.NewReader(gzipTar(t, map[string]string{"d1/d2/up/link": "-> ../../x"})), dest)
	if err == nil || !strings.Contains(err.Error(), "a symlink leaving the folder") {
		t.Errorf("err = %v, want the link refused", err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "link")); err == nil {
		t.Error("the link landed, pointing outside the destination")
	}

	// Through the same symlink, a target that stays inside on disk lands.
	if _, err := untar(bytes.NewReader(gzipTar(t, map[string]string{"d1/d2/up/ok": "-> d1"})), dest); err != nil {
		t.Errorf("an inside link through an inside symlink: %v", err)
	}
	if got, err := os.Readlink(filepath.Join(dest, "ok")); err != nil || got != "d1" {
		t.Errorf("ok -> %q, %v", got, err)
	}
}

// A link's target is stored clean, so a component that is itself a
// symlink (here one the same archive made) cannot carry a later .. above
// the destination: "a/../escape" through a -> . resolves to dest/../escape
// as written, and to dest/escape as stored.
func TestUntarStoresALinkTargetClean(t *testing.T) {
	base := t.TempDir()
	dest := filepath.Join(base, "dest")
	if _, err := untar(bytes.NewReader(gzipTar(t, map[string]string{"a": "-> .", "b": "-> a/../escape"})), dest); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(filepath.Join(dest, "b")); err != nil || got != "escape" {
		t.Errorf("b -> %q, %v; want the clean target", got, err)
	}
	if err := os.WriteFile(filepath.Join(base, "escape"), []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "b")); err == nil {
		t.Errorf("b reads the file beside the destination: %q", got)
	}

	// A target that goes on through a symlink the destination already held,
	// one leading outside, is refused.
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dest, "out")); err != nil {
		t.Fatal(err)
	}
	_, err := untar(bytes.NewReader(gzipTar(t, map[string]string{"c": "-> out/secret"})), dest)
	if err == nil || !strings.Contains(err.Error(), "a symlink in the destination leads outside it") {
		t.Errorf("err = %v, want the link through the escaping symlink refused", err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "c")); err == nil {
		t.Error("c landed")
	}
}

// A link through a link the same archive made, whose own target comes
// later, lands: that link is inside by construction. One through a
// dangling symlink the destination already held is refused, since where
// it leads is unknown.
func TestUntarFollowsItsOwnLinksForward(t *testing.T) {
	dest := t.TempDir()
	if _, err := untar(bytes.NewReader(gzipTar(t, map[string]string{"a": "-> z", "b": "-> a/x", "z/": "", "z/x": "hi\n"})), dest); err != nil {
		t.Fatalf("a forward link through the archive's own link: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "b")); err != nil || string(got) != "hi\n" {
		t.Errorf("b reads %q, %v", got, err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), filepath.Join(dest, "dangling")); err != nil {
		t.Fatal(err)
	}
	_, err := untar(bytes.NewReader(gzipTar(t, map[string]string{"c": "-> dangling/x"})), dest)
	if err == nil || !strings.Contains(err.Error(), "does not resolve") {
		t.Errorf("err = %v, want the dangling symlink named", err)
	}
}

// A hard link in the export is unpacked as the file it links to (an export
// of a tree with hard links names the second and later paths that way),
// held to a file this archive unpacked; anything else is refused. pull
// says which entries it left out rather than dropping them silently.
func TestFsPullCarriesHardLinksAndNamesWhatItSkips(t *testing.T) {
	engine, b := fsBench(t)
	engine.script(func(f *fsEngine) {
		f.export["app"] = map[string]string{"a.txt": "shared\n", "b.txt": "=> a.txt", "pipe": "|fifo"}
	})
	dest := filepath.Join(b.project, "hl")
	code, _, stderr := runSandboxCLI(t, "sandbox", "fs", "pull", "app", "--dest", dest)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "b.txt")); err != nil || string(got) != "shared\n" {
		t.Errorf("b.txt = %q, %v", got, err)
	}
	sbInOrder(t, stderr, "pipe is neither a file, a directory nor a link; left out", "✓ Pulled app: 2 files")

	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "secret"), []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest2 := filepath.Join(base, "dest")
	if err := os.MkdirAll(dest2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest2, "there"), []byte("already\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, entries := range map[string]map[string]string{
		"leaving":   {"h": "=> ../secret"},
		"absolute":  {"h": "=> /etc/passwd"},
		"not ours":  {"h": "=> there"},
		"a symlink": {"a": "-> there", "h": "=> a"},
	} {
		_, err := untar(bytes.NewReader(gzipTar(t, entries)), dest2)
		if err == nil || !strings.Contains(err.Error(), "is refused") {
			t.Errorf("%s: err = %v, want refused", name, err)
		}
		if _, err := os.Lstat(filepath.Join(dest2, "h")); err == nil {
			t.Errorf("%s: h landed", name)
		}
	}
}

// A file already in the destination is replaced, not written in place: a
// hard link to a file outside keeps the outside copy as it was.
func TestUntarReplacesAHardLinkedFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hard links")
	}
	base := t.TempDir()
	dest := filepath.Join(base, "dest")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside.txt")
	if err := os.WriteFile(outside, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(dest, "f.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := untar(bytes.NewReader(gzipTar(t, map[string]string{"f.txt": "new\n"})), dest); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(outside); string(got) != "keep\n" {
		t.Errorf("the hard-linked file outside was written: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "f.txt")); string(got) != "new\n" {
		t.Errorf("f.txt = %q", got)
	}
	entries, _ := os.ReadDir(dest)
	if len(entries) != 1 {
		t.Errorf("dest holds %v, want only f.txt (no temp file left)", entries)
	}
}

// A file pulled over one already there takes the archive's mode, the
// executable bit included, both ways.
func TestUntarAppliesTheArchivedMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix modes")
	}
	dest := t.TempDir()
	for name, mode := range map[string]os.FileMode{"run.sh": 0o755, "notes.md": 0o644} {
		if err := os.WriteFile(filepath.Join(dest, name), []byte("old\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(dest, name), mode); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range []struct {
		name string
		mode int64
	}{{"notes.md", 0o755}, {"run.sh", 0o644}} {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: e.mode, Typeflag: tar.TypeReg, Size: 4}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte("new\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := untar(&buf, dest); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]os.FileMode{"run.sh": 0o644, "notes.md": 0o755} {
		fi, err := os.Stat(filepath.Join(dest, name))
		if err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s: mode %v, %v; want %v", name, fi.Mode().Perm(), err, want)
		}
	}
}

// An export that stalls mid-stream is cut at fsTransferTimeout: the
// stream API carries no client timeout of its own, so the pull's context
// is what bounds it.
func TestFsPullGivesUpOnAStalledExport(t *testing.T) {
	engine, b := fsBench(t)
	fsTransferTimeout = 300 * time.Millisecond
	t.Cleanup(func() { fsTransferTimeout = 10 * time.Minute })
	engine.script(func(f *fsEngine) {
		// Enough bytes that half the gzip is past its header.
		f.export["app"]["big.txt"] = strings.Repeat("0123456789abcdef", 4096)
		f.exportStall = true
	})
	start := time.Now()
	code, _, stderr := runSandboxCLI(t, "sandbox", "fs", "pull", "app", "--dest", filepath.Join(b.project, "stalled"))
	if took := time.Since(start); took >= exportStallMax-time.Second {
		t.Errorf("the pull waited %s on a stalled export; want it cut near %s", took, fsTransferTimeout)
	}
	if code != 1 || !strings.Contains(stderr, "deadline exceeded") {
		t.Errorf("exit %d:\n%s", code, stderr)
	}
}

func TestFsPushPutsEveryFile(t *testing.T) {
	engine, b := fsBench(t)
	src := filepath.Join(b.project, "work")
	for name, body := range map[string]string{"README.md": "hello\n", "src/main.py": "print(2)\n", ".git/HEAD": "ref\n", ".veris/twin.local.yaml": "secret\n"} {
		p := filepath.Join(src, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("README.md", filepath.Join(src, "link.md")); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runSandboxCLI(t, "sandbox", "fs", "push", "app", "--src", src)
	if code != 0 || stdout != "" {
		t.Fatalf("exit %d, stdout %q:\n%s", code, stdout, stderr)
	}
	sbInOrder(t, stderr, "✓ Pushed "+src+" → app: 3 files, 24 B", "→ veris sandbox fs diff app")
	calls := engine.davCalls()
	seen := map[string]davCall{}
	for _, c := range calls {
		seen[c.Method+" "+c.Path] = c
		if c.Auth != fsUser+":"+fsPass {
			t.Errorf("%s %s carried auth %q, want the minted credential", c.Method, c.Path, c.Auth)
		}
	}
	for _, want := range []string{"MKCOL app/src/", "PUT app/README.md", "PUT app/src/main.py", "PUT app/link.md.rclonelink", "MKCOL app/.veris/"} {
		if _, ok := seen[want]; !ok {
			t.Errorf("missing %q in %+v", want, calls)
		}
	}
	if seen["PUT app/src/main.py"].Body != "print(2)\n" || seen["PUT app/link.md.rclonelink"].Body != "README.md" {
		t.Errorf("bodies: %+v", seen)
	}
	for _, absent := range []string{"MKCOL app/.git/", "PUT app/.git/HEAD", "PUT app/.veris/twin.local.yaml"} {
		if _, ok := seen[absent]; ok {
			t.Errorf("%q must stay behind", absent)
		}
	}
	if got := engine.minted(); len(got) != 1 {
		t.Errorf("push mints one credential, got %v", got)
	}

	if code, _, stderr := runSandboxCLI(t, "sandbox", "fs", "push", "app", "--src", filepath.Join(b.project, "nope")); code != 1 || !strings.Contains(stderr, "is not a directory to push") {
		t.Errorf("exit %d:\n%s", code, stderr)
	}
}

// A path that changes kind between the folder and the source is replaced,
// not left beside its other spelling: a file that became a symlink loses
// the file (the engine reads x.rclonelink as a link only while x is
// absent), a symlink that became a file or directory loses its marker. A
// directory that became a symlink is refused rather than deleted.
func TestFsPushReplacesAPathThatChangedKind(t *testing.T) {
	engine, b := fsBench(t)
	engine.script(func(f *fsEngine) {
		f.remote = map[string]bool{"app": true, "app/x": false, "app/y.rclonelink": false, "app/z.rclonelink": false, "app/keep": false}
	})
	src := filepath.Join(b.project, "work")
	if err := os.MkdirAll(filepath.Join(src, "z"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"README.md": "hi\n", "y": "now a file\n", "z/f": "in z\n"} {
		if err := os.WriteFile(filepath.Join(src, filepath.FromSlash(name)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("README.md", filepath.Join(src, "x")); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runSandboxCLI(t, "sandbox", "fs", "push", "app", "--src", src)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	var calls []string
	for _, c := range engine.davCalls() {
		if c.Method != "PROPFIND" {
			calls = append(calls, c.Method+" "+c.Path)
		}
	}
	got := strings.Join(calls, "\n") + "\n"
	sbInOrder(t, got, "DELETE app/x\n", "PUT app/x.rclonelink\n")
	sbInOrder(t, got, "DELETE app/y.rclonelink\n", "PUT app/y\n")
	sbInOrder(t, got, "DELETE app/z.rclonelink\n", "MKCOL app/z/\n")
	if strings.Contains(got, "DELETE app/keep") || strings.Contains(got, "DELETE app/README.md") {
		t.Errorf("push deleted what the source did not replace:\n%s", got)
	}

	// A directory named like the marker of a path the source has is not
	// a marker; it is refused, never deleted.
	engine.script(func(f *fsEngine) {
		f.remote = map[string]bool{"app": true, "app/y.rclonelink": true, "app/y.rclonelink/inner": false}
		f.dav = nil
	})
	code, _, stderr = runSandboxCLI(t, "sandbox", "fs", "push", "app", "--src", src)
	if code != 1 || !strings.Contains(stderr, "y.rclonelink is a directory in the folder") {
		t.Errorf("exit %d:\n%s", code, stderr)
	}
	for _, c := range engine.davCalls() {
		if c.Method == http.MethodDelete {
			t.Errorf("deleted %s", c.Path)
		}
	}

	// A directory in the folder where the source has a symlink: refused,
	// and the directory is not deleted.
	engine.script(func(f *fsEngine) {
		f.remote = map[string]bool{"app": true, "app/d": true, "app/d/inner": false}
		f.dav = nil
	})
	if err := os.Symlink("README.md", filepath.Join(src, "d")); err != nil {
		t.Fatal(err)
	}
	code, _, stderr = runSandboxCLI(t, "sandbox", "fs", "push", "app", "--src", src)
	if code != 1 || !strings.Contains(stderr, "d is a directory in the folder and a symlink here") {
		t.Errorf("exit %d:\n%s", code, stderr)
	}
	for _, c := range engine.davCalls() {
		if c.Method == http.MethodDelete && strings.HasPrefix(c.Path, "app/d") {
			t.Errorf("the directory was deleted: %+v", c)
		}
	}
}

// A source named through a symlink (`current -> releases/v3`) pushes what
// it points at: WalkDir reads its root with Lstat, and an unresolved root
// would be one link entry and "0 files" reported as success.
func TestFsPushFollowsASymlinkedSource(t *testing.T) {
	engine, b := fsBench(t)
	real := filepath.Join(b.project, "releases", "v3")
	if err := os.MkdirAll(filepath.Join(real, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"README.md": "hello\n", "src/main.py": "print(3)\n"} {
		if err := os.WriteFile(filepath.Join(real, filepath.FromSlash(name)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	current := filepath.Join(b.project, "current")
	if err := os.Symlink(real, current); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runSandboxCLI(t, "sandbox", "fs", "push", "app", "--src", current)
	if code != 0 || stdout != "" {
		t.Fatalf("exit %d, stdout %q:\n%s", code, stdout, stderr)
	}
	sbInOrder(t, stderr, "✓ Pushed "+current+" → app: 2 files, 15 B")
	seen := map[string]davCall{}
	for _, c := range engine.davCalls() {
		seen[c.Method+" "+c.Path] = c
	}
	if seen["PUT app/README.md"].Body != "hello\n" || seen["PUT app/src/main.py"].Body != "print(3)\n" {
		t.Errorf("the symlinked source's files never arrived: %+v", seen)
	}
	if _, ok := seen["MKCOL app/src/"]; !ok {
		t.Errorf("missing MKCOL app/src/ in %+v", seen)
	}
}

func TestFsVerbsWithoutAFilesystemOrOnAnOldEngine(t *testing.T) {
	plane := newSandboxPlane(t)
	twins := newDataTwins(t)
	engine := newFSEngine(t)
	dataBench(t, plane, twins.services())
	for _, verb := range [][]string{{"connect"}, {"diff"}, {"pull"}, {"push"}, {"mount", "/tmp/x"}, {"attach"}} {
		code, stdout, stderr := runSandboxCLI(t, append([]string{"sandbox", "fs"}, verb...)...)
		if code != 1 || stdout != "" {
			t.Errorf("%v: exit %d, stdout %q:\n%s", verb, code, stdout, stderr)
		}
		sbInOrder(t, stderr, "✗ Sandbox "+sbID+" has no filesystem", "→ Next: veris up --fs [NAME=]SOURCE[@REF][:root]")
	}

	engine.script(func(f *fsEngine) { f.old = true })
	plane.script(func(p *sandboxPlane) {
		services := append(twins.services(), engine.service())
		p.answer = func(int) *api.Sandbox { return readySandbox(services, time.Now().Add(time.Hour)) }
	})
	for _, verb := range [][]string{{"connect"}, {"diff"}, {"pull"}, {"push"}, {"attach"}} {
		code, stdout, stderr := runSandboxCLI(t, append([]string{"sandbox", "fs"}, verb...)...)
		if code != 1 || stdout != "" || !strings.Contains(stderr, "✗ The control plane does not serve filesystem routes for sandbox "+sbID+" (upgrade services-sandbox)") {
			t.Errorf("%v: exit %d, stdout %q:\n%s", verb, code, stdout, stderr)
		}
	}
}

func TestRcloneArgsPerOS(t *testing.T) {
	got, err := rcloneArgs("darwin", ":webdav:app/", "/mnt/app", false)
	if err != nil || strings.Join(got, " ") != "nfsmount :webdav:app/ /mnt/app --vfs-cache-mode full --links" {
		t.Errorf("darwin: %q, %v", got, err)
	}
	got, err = rcloneArgs("linux", ":webdav:", "/mnt/fs", true)
	if err != nil || strings.Join(got, " ") != "mount :webdav: /mnt/fs --vfs-cache-mode full --links --daemon" {
		t.Errorf("linux: %q, %v", got, err)
	}
	if _, err := rcloneArgs("windows", ":webdav:", `C:\fs`, false); err == nil {
		t.Error("windows mounted")
	}
	obscured, err := rcloneObscure("p_secret_1")
	if err != nil {
		t.Fatal(err)
	}
	if back, err := rcloneReveal(obscured); err != nil || back != "p_secret_1" {
		t.Errorf("reveal(obscure) = %q, %v", back, err)
	}
	if strings.ContainsAny(obscured, "+/=") {
		t.Errorf("obscured %q is not URL-safe base64 without padding", obscured)
	}
	env := rcloneEnv("https://gw/s/x/filesystem/", "u", obscured)
	if len(env) != 4 || env[0] != "RCLONE_WEBDAV_URL=https://gw/s/x/filesystem/" || env[1] != "RCLONE_WEBDAV_VENDOR=other" || env[2] != "RCLONE_WEBDAV_USER=u" || env[3] != "RCLONE_WEBDAV_PASS="+obscured {
		t.Errorf("env = %q", env)
	}
}

func TestFsMountNeedsRclone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mount is refused outright on Windows")
	}
	engine, b := fsBench(t)
	was := lookPath
	lookPath = func(string) (string, error) { return "", os.ErrNotExist }
	t.Cleanup(func() { lookPath = was })
	code, stdout, stderr := runSandboxCLI(t, "sandbox", "fs", "mount", filepath.Join(b.project, "mnt"))
	if code != 1 || stdout != "" {
		t.Fatalf("exit %d, stdout %q:\n%s", code, stdout, stderr)
	}
	sbInOrder(t, stderr, "✗ rclone is not on PATH; mount needs it", "→ Next: brew install rclone  (or https://rclone.org/install/)")
	if len(engine.minted()) != 0 {
		t.Error("a credential was minted for a mount that cannot happen")
	}
	if code, _, stderr := runSandboxCLI(t, "sandbox", "fs", "attach", "--mount", "app"); code != 1 || !strings.Contains(stderr, "rclone is not on PATH") {
		t.Errorf("attach --mount: exit %d:\n%s", code, stderr)
	}
}

// attach lays the folders out as the pod does -- the root folder is DIR
// itself, the others DIR/<name> -- copies what is not --mount'ed, mounts
// the rest through rclone with the credential in its environment, and
// prints the exports an agent run here reads.
func TestFsAttachLaysTheWorkspaceOut(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake rclone is a shell script")
	}
	engine, b := fsBench(t)
	engine.script(func(f *fsEngine) {
		f.layout.Folders = append(f.layout.Folders, twin.FSFolder{Name: "data", Source: twin.FSSource{Kind: "upload", ID: "up_1"}, Ready: true})
		f.export["data"] = map[string]string{"rows.csv": "1,2\n"}
	})
	dir := filepath.Join(b.project, "ws")
	code, stdout, stderr := runSandboxCLI(t, "sandbox", "fs", "attach", dir)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	sbInOrder(t, stderr, "✓ Pulled app: 2 files, 12 B → "+dir+"\n", "✓ Pulled data: 1 file, 4 B → "+filepath.Join(dir, "data")+"\n", "Attached the filesystem of "+sbID+" under "+dir)
	if got, _ := os.ReadFile(filepath.Join(dir, "README.md")); string(got) != "hi\n" {
		t.Errorf("the root folder is DIR itself; README.md = %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "data", "rows.csv")); string(got) != "1,2\n" {
		t.Errorf("data/rows.csv = %q", got)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 || lines[0] != "export WORKSPACE_DIR='"+dir+"'" || !strings.HasPrefix(lines[1], "export WORKSPACE_FOLDERS='[") {
		t.Fatalf("stdout:\n%s", stdout)
	}
	var folders []attachedFolder
	raw := strings.TrimSuffix(strings.TrimPrefix(lines[1], "export WORKSPACE_FOLDERS='"), "'")
	if err := json.Unmarshal([]byte(raw), &folders); err != nil || len(folders) != 2 {
		t.Fatalf("WORKSPACE_FOLDERS: %v\n%s", err, raw)
	}
	if folders[0].Name != "app" || folders[0].Path != dir || folders[0].Attach != "copy" || folders[0].SHA != "abc123" || folders[0].Source.URL != "https://github.com/acme/app" {
		t.Errorf("app row = %+v", folders[0])
	}
	if folders[1].Name != "data" || folders[1].Path != filepath.Join(dir, "data") || folders[1].Source.ID != "up_1" {
		t.Errorf("data row = %+v", folders[1])
	}

	// The root folder is DIR itself; mounting it would hide the others.
	code, stdout, stderr = runSandboxCLI(t, "sandbox", "fs", "attach", filepath.Join(b.project, "ws-root"), "--mount", "app")
	if code != 1 || stdout != "" {
		t.Fatalf("exit %d, stdout %q:\n%s", code, stdout, stderr)
	}
	sbInOrder(t, stderr, "✗ app is the root folder, so the other folders land inside it; mounting it would hide them", "→ Next: veris sandbox fs mount app DIR")
	if _, err := os.Stat(filepath.Join(b.project, "ws-root")); err == nil {
		t.Error("something was copied before the refusal")
	}

	// --mount data: the fake rclone records what it was asked and told.
	record := filepath.Join(b.project, "rclone.log")
	fake := filepath.Join(b.project, "bin", "rclone")
	if err := os.MkdirAll(filepath.Dir(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n{ echo \"args: $*\"; env | grep '^RCLONE_' | sort; } > " + record + "\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	was := lookPath
	lookPath = func(string) (string, error) { return fake, nil }
	t.Cleanup(func() { lookPath = was })
	dir2 := filepath.Join(b.project, "ws2")
	code, stdout, stderr = runSandboxCLI(t, "sandbox", "fs", "attach", dir2, "--mount", "data", "--ttl", "2h")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	// The unmount hint is the OS's own: fusermount on Linux, umount elsewhere.
	unmount := "umount "
	if runtime.GOOS == "linux" {
		unmount = "fusermount -u "
	}
	sbInOrder(t, stderr, "✓ Pulled app:", "✓ Mounted data at "+filepath.Join(dir2, "data"), "→ Next: "+unmount)
	if !strings.Contains(stdout, `"attach":"mount"`) {
		t.Errorf("WORKSPACE_FOLDERS should mark data as a mount:\n%s", stdout)
	}
	logged, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the fake rclone never ran: %v", err)
	}
	sub := "nfsmount"
	if runtime.GOOS == "linux" {
		sub = "mount"
	}
	sbInOrder(t, string(logged),
		"args: "+sub+" :webdav:data/ "+filepath.Join(dir2, "data")+" --vfs-cache-mode full --links --daemon\n",
		"RCLONE_WEBDAV_PASS=", "RCLONE_WEBDAV_URL="+engine.srv.URL+"/s/"+sbID+"/filesystem/\n", "RCLONE_WEBDAV_USER=u_1\n", "RCLONE_WEBDAV_VENDOR=other\n")
	for _, line := range strings.Split(string(logged), "\n") {
		if obscured, ok := strings.CutPrefix(line, "RCLONE_WEBDAV_PASS="); ok {
			if back, err := rcloneReveal(obscured); err != nil || back != fsPass {
				t.Errorf("the mount's password does not reveal to the minted one: %q, %v", back, err)
			}
		}
	}
	if strings.Contains(string(logged), fsPass) {
		t.Error("the plaintext password reached rclone's environment")
	}
	if got := engine.minted(); got[len(got)-1] != 7200 {
		t.Errorf("the mount's ttl_s = %v, want 7200 last", got)
	}
}

// davList reads a Depth: 1 listing into child names, skipping the
// collection itself whether its href carries the proxy's prefix or not.
func TestDavListSkipsTheCollectionWhateverItsHref(t *testing.T) {
	for name, prefix := range map[string]string{"full": "/s/sb/filesystem", "prefix-less": ""} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "PROPFIND" || r.Header.Get("Depth") != "1" {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.WriteHeader(http.StatusMultiStatus)
				_, _ = io.WriteString(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:">`+
					`<d:response><d:href>`+prefix+`/app/sub/</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop></d:propstat></d:response>`+
					`<d:response><d:href>`+prefix+`/app/sub/a%20b.txt</d:href><d:propstat><d:prop><d:resourcetype/></d:prop></d:propstat></d:response>`+
					`<d:response><d:href>`+prefix+`/app/sub/sub/</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop></d:propstat></d:response>`+
					`</d:multistatus>`)
			}))
			defer srv.Close()
			got, err := davList(context.Background(), srv.Client(), srv.URL+"/s/sb/filesystem/app/sub/", &twin.FSToken{User: "u", Password: "p"})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 2 || got["a b.txt"] || !got["sub"] {
				t.Errorf("listing = %v, want a b.txt (file) and sub (collection)", got)
			}
		})
	}
}

// The WebDAV root takes the same same-host HTTPS upgrade as the control
// URL: an HTTPS plane that advertises http:// for its own host must not
// have push send the minted credential in the clear (or mount, or connect
// export it).
func TestDavURLIsUpgradedLikeTheControlURL(t *testing.T) {
	for _, c := range []struct{ plane, url, want string }{
		{"https://api.veris.ai", "http://api.veris.ai/s/sb/filesystem", "https://api.veris.ai/s/sb/filesystem/"},
		{"https://api.veris.ai", "https://api.veris.ai/s/sb/filesystem/", "https://api.veris.ai/s/sb/filesystem/"},
		{"https://api.veris.ai", "http://elsewhere.example/s/sb/filesystem", "http://elsewhere.example/s/sb/filesystem/"},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080/s/sb/filesystem", "http://127.0.0.1:8080/s/sb/filesystem/"},
	} {
		f := &fsSession{s: &session{res: &cfg.Resolved{APIBase: c.plane}}, svc: &api.ServiceInfo{URL: c.url}}
		if got := f.davURL(); got != c.want {
			t.Errorf("plane %s, url %s: davURL = %s, want %s", c.plane, c.url, got, c.want)
		}
	}
}

// A diff that stalls is cut at the JSON budget's own deadline rather than
// waiting forever: only the export stream runs without a client timeout.
func TestFsDiffGivesUpOnAStalledEngine(t *testing.T) {
	was := fsDiffTimeout
	fsDiffTimeout = 300 * time.Millisecond
	t.Cleanup(func() { fsDiffTimeout = was })
	engine, _ := fsBench(t)
	engine.script(func(f *fsEngine) { f.diffStall = true })
	start := time.Now()
	code, _, stderr := runSandboxCLI(t, "sandbox", "fs", "diff", "app")
	if code != 1 || !strings.Contains(stderr, "deadline exceeded") {
		t.Errorf("exit %d:\n%s", code, stderr)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("diff waited %s on a stalled engine", took)
	}
}

// The root folder's own copy of a path another folder lands on is left
// out, as the pod's mount of that folder shadows it: a copied folder is
// not merged with stale files, and a mounted one gets an empty mountpoint
// (rclone refuses one that is not).
func TestFsAttachLeavesTheRootsCopyOfAnotherFolderOut(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake rclone is a shell script")
	}
	engine, b := fsBench(t)
	engine.script(func(f *fsEngine) {
		f.layout.Folders = append(f.layout.Folders,
			twin.FSFolder{Name: "data", Source: twin.FSSource{Kind: "upload", ID: "up_1"}, Ready: true},
			twin.FSFolder{Name: "cache", Source: twin.FSSource{Kind: "upload", ID: "up_2"}, Ready: true})
		f.export["app"] = map[string]string{"README.md": "hi\n", "data/": "", "data/stale.txt": "root's\n", "cache/": "", "cache/old.bin": "root's\n", "keep/": "", "keep/x": "x\n"}
		f.export["data"] = map[string]string{"rows.csv": "1,2\n"}
	})
	fake := filepath.Join(b.project, "bin", "rclone")
	if err := os.MkdirAll(filepath.Dir(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	// The fake mount fails on a mountpoint that is not empty, as rclone does.
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nfor a; do case $a in /*) mp=$a;; esac; done\n[ -z \"$(ls -A \"$mp\" 2>/dev/null)\" ] || { echo \"mountpoint $mp is not empty\"; exit 1; }\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	was := lookPath
	lookPath = func(string) (string, error) { return fake, nil }
	t.Cleanup(func() { lookPath = was })
	dir := filepath.Join(b.project, "ws")
	code, _, stderr := runSandboxCLI(t, "sandbox", "fs", "attach", dir, "--mount", "cache")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "data", "stale.txt")); err == nil {
		t.Error("the root's copy of data/ was merged into the data folder")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "data", "rows.csv")); string(got) != "1,2\n" {
		t.Errorf("data/rows.csv = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "cache", "old.bin")); err == nil {
		t.Error("the root's copy of cache/ filled the mountpoint")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "keep", "x")); string(got) != "x\n" {
		t.Errorf("keep/x = %q; the rest of the root must land", got)
	}
	sbInOrder(t, stderr, "app: data/ is left out; folder data lands there, as in the pod")
}
