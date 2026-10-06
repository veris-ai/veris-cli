package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/veris-ai/veris-cli/internal/api"
	"github.com/veris-ai/veris-cli/internal/cfg"
	"github.com/veris-ai/veris-cli/internal/cli"
)

// The grammar's delimiters collide with git's own spelling, so each case
// here is a shape a user will type: scp-style URLs keep their user and
// path, a ref is read only after the repository, :root comes off first,
// and a local directory must exist.
func TestParseFolderSpec(t *testing.T) {
	cwd := t.TempDir()
	for _, d := range []string{"app", "src", "fixtures", "bare.git", "My App"} {
		if err := os.Mkdir(filepath.Join(cwd, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("app", filepath.Join(cwd, "current")); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		spec    string
		want    folderSpec
		wantErr string
	}{
		{spec: "./app", want: folderSpec{Name: "app", Kind: folderPath, Source: "./app", Dir: filepath.Join(cwd, "app")}},
		{spec: "app=./src", want: folderSpec{Name: "app", Kind: folderPath, Source: "./src", Dir: filepath.Join(cwd, "src")}},
		{spec: "fx=./fixtures:root", want: folderSpec{Name: "fx", Kind: folderPath, Source: "./fixtures", Dir: filepath.Join(cwd, "fixtures"), Root: true}},
		{spec: "git@github.com:acme/app.git", want: folderSpec{Name: "app", Kind: folderGit, Source: "git@github.com:acme/app.git"}},
		{spec: "git@github.com:acme/app.git@v1.2", want: folderSpec{Name: "app", Kind: folderGit, Source: "git@github.com:acme/app.git", Ref: "v1.2"}},
		{spec: "https://github.com/acme/app@main:root", want: folderSpec{Name: "app", Kind: folderGit, Source: "https://github.com/acme/app", Ref: "main", Root: true}},
		{spec: "https://github.com/acme/app.git", want: folderSpec{Name: "app", Kind: folderGit, Source: "https://github.com/acme/app.git"}},
		{spec: "ssh://git@host:2222/org/repo.git@dev", want: folderSpec{Name: "repo", Kind: folderGit, Source: "ssh://git@host:2222/org/repo.git", Ref: "dev"}},
		{spec: "lib=https://github.com/acme/app", want: folderSpec{Name: "lib", Kind: folderGit, Source: "https://github.com/acme/app"}},
		{spec: "https://github.com/acme/app.git@feature/foo", want: folderSpec{Name: "app", Kind: folderGit, Source: "https://github.com/acme/app.git", Ref: "feature/foo"}},
		{spec: "git@github.com:acme/app.git@release/2026.1", want: folderSpec{Name: "app", Kind: folderGit, Source: "git@github.com:acme/app.git", Ref: "release/2026.1"}},
		{spec: "deploy@git.acme.io:org/app.git@v3", want: folderSpec{Name: "app", Kind: folderGit, Source: "deploy@git.acme.io:org/app.git", Ref: "v3"}},
		// The control plane refuses a credential in the URL (it is recorded
		// in the clear) and fetches only https and ssh; both are said here,
		// before any local folder is uploaded. The user part of the URL is
		// still read as the URL's own, not as a ref's @.
		{spec: "https://user@github.com/acme/app.git", wantErr: "a credential does not belong in the git URL https://xxxxx@github.com/acme/app.git"},
		{spec: "https://user:tok@github.com/acme/app.git@v3", wantErr: "a credential does not belong in the git URL https://user:xxxxx@github.com/acme/app.git; name an environment secret with auth:"},
		{spec: "http://github.com/acme/app", wantErr: "is not a git URL the sandbox can fetch"},
		{spec: "git://github.com/acme/app.git", wantErr: "(http:// and git:// are not fetched)"},
		{spec: "https://github.com", wantErr: "is not a repository URL"},
		{spec: "github.com/acme/app.git", wantErr: "is not a git URL the sandbox can fetch"},
		// A local directory whose name ends in .git is a directory.
		{spec: "./bare.git", want: folderSpec{Name: "bare.git", Kind: folderPath, Source: "./bare.git", Dir: filepath.Join(cwd, "bare.git")}},
		// A name the control plane would refuse is refused before the upload.
		{spec: "./My App", wantErr: `folder name "My App" must start with a letter or digit and hold only letters, digits, '.', '_' and '-'`},
		{spec: "app=./My App", want: folderSpec{Name: "app", Kind: folderPath, Source: "./My App", Dir: filepath.Join(cwd, "My App")}},
		{spec: ".hidden=", wantErr: `folder name ".hidden" must start with a letter or digit`},
		// A symlink to a directory is a directory to the flag; the upload
		// follows it (TestTarDirFollowsASymlinkedSource).
		{spec: "./current", want: folderSpec{Name: "current", Kind: folderPath, Source: "./current", Dir: filepath.Join(cwd, "current")}},
		// The control plane refuses a gs:// folder without a secret, and the
		// flag has nowhere to name one: the config does.
		{spec: "gs://bucket/prefix", wantErr: "--fs gs://bucket/prefix: a gs:// source needs auth: naming the secret the engine reads it with, which --fs cannot carry; put it in the config under filesystem: as {gcs: gs://bucket/prefix, auth: SECRET_NAME}"},
		{spec: "data=gs://b/p:root", wantErr: "--fs data=gs://b/p:root: a gs:// source needs auth:"},
		{spec: "scratch=", want: folderSpec{Name: "scratch", Kind: folderEmpty}},
		{spec: "./missing", wantErr: "--fs ./missing: no such directory"},
		{spec: "", wantErr: "an empty folder needs a name"},
		{spec: "a/b=./app", wantErr: "no such directory"},
		{spec: "..=./app", wantErr: `folder name ".." is not a name`},
	}
	for _, tc := range cases {
		t.Run(tc.spec, func(t *testing.T) {
			got, err := parseFolderSpec(tc.spec, cwd)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got  %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

// A credential typed into --fs or the config is refused, and no error
// says it back: not the git check's own words, not the "--fs SPEC:" prefix
// in front of them, on any path a credential can reach.
func TestFolderSpecErrorsNeverEchoACredential(t *testing.T) {
	cwd := t.TempDir()
	for _, spec := range []string{
		"https://user:s3cret@github.com/acme/app.git@v3",
		"app=https://user:s3cret@github.com/acme/app.git:root",
		"https://s3cret@github.com/acme/app",
		"https://x-access-token:s3cret@github.com",
		"http://user:s3cret@github.com/acme/app",
		"git://user:s3cret@github.com/acme/app.git",
		"ssh://git:s3cret@host/org/repo.git",
		"ssh://git:s3cret@host",
		"user:s3cret@host/org/repo",
		"BAD NAME=https://user:s3cret@github.com/acme/app",
		"bad..=https://user:s3cret@github.com/acme/app",
	} {
		_, err := foldersFromFlags([]string{spec}, cwd)
		if err == nil {
			t.Errorf("%s: accepted", spec)
			continue
		}
		if strings.Contains(err.Error(), "s3cret") {
			t.Errorf("the error says the credential back: %v", err)
		}
	}
	for _, git := range []string{"https://me:s3cret@h/app", "https://s3cret@h/app", "http://me:s3cret@h/app", "https://me:s3cret@h"} {
		_, err := foldersFromConfig(cwd, []cfg.FolderConfig{{Git: git}})
		if err == nil {
			t.Errorf("%s: accepted", git)
			continue
		}
		if strings.Contains(err.Error(), "s3cret") {
			t.Errorf("the error says the credential back: %v", err)
		}
	}
	if got := redactCredentials("ssh://git@host/org/repo.git"); got != "ssh://git@host/org/repo.git" {
		t.Errorf("an ssh user is not a credential: %s", got)
	}
	if got := redactCredentials("git@github.com:acme/app.git@v1"); got != "git@github.com:acme/app.git@v1" {
		t.Errorf("an scp-style user is not a credential: %s", got)
	}
}

func TestFoldersFromFlagsHoldTheServersRules(t *testing.T) {
	cwd := t.TempDir()
	if err := os.Mkdir(filepath.Join(cwd, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	var usage *cli.UsageError
	_, err := foldersFromFlags([]string{"./app", "https://github.com/acme/app"}, cwd)
	if !errors.As(err, &usage) || !strings.Contains(err.Error(), `two folders are named "app"`) {
		t.Errorf("duplicate names: %v", err)
	}
	_, err = foldersFromFlags([]string{"./app:root", "https://h/lib:root"}, cwd)
	if !errors.As(err, &usage) || !strings.Contains(err.Error(), "both app and lib are :root") {
		t.Errorf("two roots: %v", err)
	}
	_, err = foldersFromFlags([]string{"./nope"}, cwd)
	if !errors.As(err, &usage) {
		t.Errorf("a bad spec is a usage error, got %v", err)
	}
	_, err = foldersFromFlags([]string{"./app", "gs://b/p"}, cwd)
	if !errors.As(err, &usage) || !strings.Contains(err.Error(), "a gs:// source needs auth:") {
		t.Errorf("a gs:// source is a usage error naming auth:, got %v", err)
	}
	got, err := foldersFromFlags([]string{"./app:root", "https://h/lib"}, cwd)
	if err != nil || len(got) != 2 || !got[0].Root || got[1].Name != "lib" {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestFoldersFromConfigAndTheRequest(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "fixtures", "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	folders, err := foldersFromConfig(dir, []cfg.FolderConfig{
		{Git: "git@github.com:acme/app.git", Ref: "main", Root: true, Auth: "GH_KEY"},
		{Path: "fixtures/data"},
		{GCS: "gs://bucket/prefix", Auth: "GCS_SA"},
		{Name: "scratch"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []folderSpec{
		{Name: "app", Kind: folderGit, Source: "git@github.com:acme/app.git", Ref: "main", Root: true, Auth: "GH_KEY"},
		{Name: "data", Kind: folderPath, Source: "fixtures/data", Dir: filepath.Join(dir, "fixtures", "data")},
		{Name: "prefix", Kind: folderGCS, Source: "gs://bucket/prefix", Auth: "GCS_SA"},
		{Name: "scratch", Kind: folderEmpty},
	}
	if !reflect.DeepEqual(folders, want) {
		t.Errorf("got  %+v\nwant %+v", folders, want)
	}
	req := folderRequest(folders, map[string]string{"data": "up_1"})
	wantReq := []api.FilesystemFolder{
		{Name: "app", Git: "git@github.com:acme/app.git", Ref: "main", Root: true, Auth: "GH_KEY"},
		{Name: "data", Upload: "up_1"},
		{Name: "prefix", GCS: "gs://bucket/prefix", Auth: "GCS_SA"},
		{Name: "scratch"},
	}
	if !reflect.DeepEqual(req, wantReq) {
		t.Errorf("request\n got  %+v\n want %+v", req, wantReq)
	}

	for _, bad := range []struct {
		conf []cfg.FolderConfig
		want string
	}{
		{[]cfg.FolderConfig{{Git: "g", Path: "fixtures"}}, "names more than one of git, gcs and path"},
		{[]cfg.FolderConfig{{Path: "nowhere"}}, "path nowhere is not a directory"},
		{[]cfg.FolderConfig{{GCS: "gs://b/p", Ref: "x", Auth: "SA"}}, "ref applies to a git source"},
		{[]cfg.FolderConfig{{GCS: "gs://b/p"}}, "filesystem[0]: gcs gs://b/p needs auth: naming the secret the engine reads it with"},
		{[]cfg.FolderConfig{{}}, "no git, gcs or path and no name"},
		{[]cfg.FolderConfig{{Git: "http://h/app"}}, "filesystem[0]: http://h/app is not a git URL the sandbox can fetch"},
		{[]cfg.FolderConfig{{Git: "https://me:tok@h/app"}}, "filesystem[0]: a credential does not belong in the git URL"},
		{[]cfg.FolderConfig{{Name: "my folder"}}, `filesystem[0]: folder name "my folder" must start with a letter or digit`},
		{[]cfg.FolderConfig{{Git: "https://h/app"}, {Path: "fixtures/data", Name: "app"}}, `two folders are named "app"`},
	} {
		if _, err := foldersFromConfig(dir, bad.conf); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%+v: err = %v, want %q", bad.conf, err, bad.want)
		}
	}
}

// The upload carries the directory as it is -- .git included, a symlink as
// a link -- minus this tool's own local file and locks, which hold sandbox
// ids.
func TestTarDirCarriesTheTreeAndLeavesTheLocalFileOut(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"README.md":                  "hi\n",
		"src/main.py":                "print(1)\n",
		".git/HEAD":                  "ref: refs/heads/main\n",
		".veris/twin.yaml":           "version: 1\n",
		".veris/twin.local.yaml":     "sandbox: secret\n",
		".veris/import.lock":         "123\n",
		"sub/.veris/twin.local.yaml": "nested secret\n",
	}
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("README.md", filepath.Join(dir, "link.md")); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	sum, err := tarDir(dir, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Files != 4 || sum.Bytes != int64(len("hi\n")+len("print(1)\n")+len("ref: refs/heads/main\n")+len("version: 1\n")) || len(sum.Skipped) != 0 {
		t.Errorf("summary = %+v", sum)
	}
	entries := tarEntries(t, buf.Bytes())
	for _, want := range []string{"README.md", "src/", "src/main.py", ".git/", ".git/HEAD", ".veris/twin.yaml", "link.md"} {
		if _, ok := entries[want]; !ok {
			t.Errorf("missing %q in %v", want, entries)
		}
	}
	for _, absent := range []string{".veris/twin.local.yaml", ".veris/import.lock", "sub/.veris/twin.local.yaml"} {
		if _, ok := entries[absent]; ok {
			t.Errorf("%q must not be uploaded", absent)
		}
	}
	if entries["link.md"] != "-> README.md" {
		t.Errorf("link.md = %q, want a symlink entry", entries["link.md"])
	}
	if entries["README.md"] != "hi\n" {
		t.Errorf("README.md = %q", entries["README.md"])
	}
}

// A source named through a symlink (`current -> releases/v3`) passes the
// flag's directory check, and WalkDir reads its root with Lstat, so without
// resolving it the upload would be one link entry and no files -- an empty
// folder in the sandbox, reported as a success. The tar follows the link.
func TestTarDirFollowsASymlinkedSource(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "releases", "v3")
	if err := os.MkdirAll(filepath.Join(real, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "src", "main.py"), []byte("print(1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "current")
	if err := os.Symlink(filepath.Join("releases", "v3"), link); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	sum, err := tarDir(link, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Files != 1 || sum.Bytes != int64(len("print(1)\n")) {
		t.Errorf("summary = %+v, want the one file behind the link", sum)
	}
	if entries := tarEntries(t, buf.Bytes()); entries["src/main.py"] != "print(1)\n" || len(entries) != 2 {
		t.Errorf("entries = %v, want src/ and src/main.py", entries)
	}
	if _, err := tarDir(filepath.Join(base, "dangling"), &buf); err == nil {
		t.Error("a directory that is not there must be an error, not an empty tar")
	}
}

// tarEntries reads a gzip tar into name → content ("-> target" for a
// symlink, "" for a directory).
func tarEntries(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		switch hdr.Typeflag {
		case tar.TypeSymlink:
			out[hdr.Name] = "-> " + hdr.Linkname
		case tar.TypeDir:
			out[hdr.Name] = ""
		default:
			body, _ := io.ReadAll(tr)
			out[hdr.Name] = string(body)
		}
	}
}

func TestByteSize(t *testing.T) {
	for n, want := range map[int64]string{72: "72 B", 640_000: "640.0 kB", 12_300_000: "12.3 MB", 2 << 30: "2.1 GB"} {
		if got := byteSize(n); got != want {
			t.Errorf("byteSize(%d) = %q, want %q", n, got, want)
		}
	}
}

// A staging archive that lands inside the tree it packs (TMPDIR under the
// source) is not packed into itself.
func TestStageFolderLeavesItsOwnArchiveOut(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(strings.Repeat("a", 1<<16)), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", dir)
	tmp, sum, err := stageFolder(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if sum.Files != 1 {
		t.Errorf("files = %d, want 1 (a.txt alone)", sum.Files)
	}
	gz, err := gzip.NewReader(tmp)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Name != "a.txt" {
			t.Errorf("packed %s", hdr.Name)
		}
	}
}
