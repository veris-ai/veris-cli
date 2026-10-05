package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/veris-ai/veris-cli/internal/api"
	"github.com/veris-ai/veris-cli/internal/cfg"
)

// fsProject writes a project whose ci environment has two folders -- a
// local directory and a git repository marked root -- and the directory
// itself, with a file this tool must never upload.
func fsProject(t *testing.T, b *bench) {
	t.Helper()
	b.projectFile(cfg.Project{
		Project: "proj",
		Default: "ci",
		Environments: map[string]cfg.EnvConfig{
			"ci": {ID: ciID, Filesystem: []cfg.FolderConfig{
				{Path: "app"},
				{Git: "https://github.com/acme/lib.git", Ref: "main", Root: true},
			}},
		},
	})
	for name, body := range map[string]string{"app/main.py": "print(1)\n", "app/.veris/twin.local.yaml": "sandbox: secret\n", "fixtures/a.json": "{}\n"} {
		p := filepath.Join(b.project, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// readyWith scripts the plane to answer a ready sandbox with services.
func readyWith(plane *sandboxPlane, services []api.ServiceInfo) {
	expires := time.Now().Add(30 * time.Minute)
	plane.script(func(p *sandboxPlane) {
		p.answer = func(int) *api.Sandbox { return readySandbox(services, expires) }
	})
}

func TestUpUploadsLocalFoldersBeforeCreate(t *testing.T) {
	plane := newSandboxPlane(t)
	twins := newSandboxTwins(t)
	engine := newFSEngine(t)
	b := sandboxBench(t, plane.srv.URL)
	fsProject(t, b)
	readyWith(plane, append(twins.services(false), engine.service()))

	code, _, stderr := runSandboxCLI(t, "up")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	davURL := engine.srv.URL + "/s/" + sbID + "/filesystem/"
	sbInOrder(t, stderr,
		"Uploading app (",
		"✓ Uploaded app: 1 file, 9 B (up_1)\n",
		"Starting 'ci' (checkout-ci: stripe, postgres) · boot bundle · ttl from the control plane · fs 2 folders\n",
		"✓ Sandbox created: "+sbID+"\n",
		"  ✓ filesystem  routable  ",
		"  filesystem   WebDAV "+davURL+"   (veris sandbox fs connect | diff | pull | push | mount)\n",
		"✓ Up: "+sbID)
	if strings.Contains(stderr, "WORKSPACE_DIR=") || strings.Contains(stderr, "ignored the filesystem") {
		t.Errorf("stderr:\n%s", stderr)
	}
	bodies, names := plane.uploadsSeen()
	if len(bodies) != 1 || names[0] != "app" {
		t.Fatalf("uploads = %d (%v), want one named app", len(bodies), names)
	}
	entries := tarEntries(t, bodies[0])
	if entries["main.py"] != "print(1)\n" {
		t.Errorf("the tar lacks main.py: %v", entries)
	}
	if _, ok := entries[".veris/twin.local.yaml"]; ok {
		t.Error("twin.local.yaml was uploaded")
	}
	if order := plane.requestOrder(); !reflect.DeepEqual(order, []string{"upload", "create"}) {
		t.Errorf("request order = %v, want the upload before the create", order)
	}
	created := plane.createdReq()
	want := []api.FilesystemFolder{
		{Name: "app", Upload: "up_1"},
		{Name: "lib", Git: "https://github.com/acme/lib.git", Ref: "main", Root: true},
	}
	if created == nil || !reflect.DeepEqual(created.Filesystem, want) {
		t.Errorf("create request filesystem = %+v, want %+v", created, want)
	}
}

func TestUpFsFlagReplacesTheConfigList(t *testing.T) {
	plane := newSandboxPlane(t)
	twins := newSandboxTwins(t)
	engine := newFSEngine(t)
	b := sandboxBench(t, plane.srv.URL)
	fsProject(t, b)
	readyWith(plane, append(twins.services(false), engine.service()))

	code, _, stderr := runSandboxCLI(t, "up", "--fs", "https://github.com/acme/lib2", "--fs", "fx=./fixtures:root")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	want := []api.FilesystemFolder{{Name: "lib2", Git: "https://github.com/acme/lib2"}, {Name: "fx", Upload: "up_1", Root: true}}
	if created := plane.createdReq(); created == nil || !reflect.DeepEqual(created.Filesystem, want) {
		t.Errorf("filesystem = %+v, want the flags' folders only", created)
	}
	if _, names := plane.uploadsSeen(); !reflect.DeepEqual(names, []string{"fx"}) {
		t.Errorf("uploaded %v, want fx only (the config's app is replaced)", names)
	}

	// A flag that will not parse is refused before anything is sent.
	code, _, stderr = runSandboxCLI(t, "up", "--fs", "./nowhere")
	if code != 1 || !strings.Contains(stderr, "veris: --fs ./nowhere: no such directory") {
		t.Errorf("exit %d:\n%s", code, stderr)
	}
	if code, _, stderr = runSandboxCLI(t, "up", "--fs", "./app:root", "--fs", "https://github.com/acme/lib2:root"); code != 1 || !strings.Contains(stderr, "both app and lib2 are :root") {
		t.Errorf("exit %d:\n%s", code, stderr)
	}
	// A gs:// source has no auth on the command line and the control plane
	// would refuse it (422) after the local folders had gone up; refused
	// here instead, with nothing uploaded and nothing created.
	plane.script(func(p *sandboxPlane) { p.uploads, p.uploadNames, p.created = nil, nil, nil })
	code, _, stderr = runSandboxCLI(t, "up", "--fs", "./fixtures", "--fs", "gs://b/p")
	if code != 1 || !strings.Contains(stderr, "veris: --fs gs://b/p: a gs:// source needs auth: naming the secret the engine reads it with, which --fs cannot carry; put it in the config under filesystem: as {gcs: gs://b/p, auth: SECRET_NAME}") {
		t.Errorf("exit %d:\n%s", code, stderr)
	}
	if _, names := plane.uploadsSeen(); len(names) != 0 || plane.createdReq() != nil {
		t.Errorf("uploaded %v, created %v; want nothing sent", names, plane.createdReq())
	}

	// Without --fs the config's own list goes, with its root.
	if code, _, stderr = runSandboxCLI(t, "up"); code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	if created := plane.createdReq(); created == nil || len(created.Filesystem) != 2 || created.Filesystem[1].Name != "lib" || !created.Filesystem[1].Root {
		t.Errorf("filesystem = %+v, want the config's two folders", created)
	}
}

// A snapshot recorded its own folders, modifications included, and a list
// sent beside snapshot_id replaces them. So with --boot snapshot the
// config's filesystem: list stays home -- nothing is uploaded, the field is
// omitted -- and only --fs, said on the command line, replaces the
// snapshot's folders.
func TestUpBootSnapshotKeepsTheSnapshotsFolders(t *testing.T) {
	plane := newSandboxPlane(t)
	twins := newSandboxTwins(t)
	engine := newFSEngine(t)
	b := sandboxBench(t, plane.srv.URL)
	fsProject(t, b) // the directories; the file is rewritten with the boot
	b.projectFile(cfg.Project{
		Project: "proj",
		Default: "ci",
		Environments: map[string]cfg.EnvConfig{
			"ci": {ID: ciID, Boot: "snapshot", Snapshot: "nightly", Filesystem: []cfg.FolderConfig{
				{Path: "app"},
				{Git: "https://github.com/acme/lib.git", Ref: "main", Root: true},
			}},
		},
	})
	snap := "snapaaaaaaaaaaaaaaaaaaaaa"
	plane.script(func(p *sandboxPlane) {
		p.snapshots = []api.Snapshot{{ID: snap, Name: "nightly", CreatedAt: at(time.Now().Add(-time.Hour))}}
	})
	readyWith(plane, append(twins.services(false), engine.service()))

	code, _, stderr := runSandboxCLI(t, "up")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	sbInOrder(t, stderr, "· boot snapshot "+shortID(snap)+" · ttl from the control plane · fs from snapshot\n", "✓ Sandbox created: "+sbID)
	if strings.Contains(stderr, "Uploading app") || strings.Contains(stderr, "fs 2 folders") {
		t.Errorf("the config's list was sent beside the snapshot:\n%s", stderr)
	}
	if _, names := plane.uploadsSeen(); len(names) != 0 {
		t.Errorf("uploaded %v, want nothing: the snapshot's folders boot", names)
	}
	if created := plane.createdReq(); created == nil || created.SnapshotID == nil || *created.SnapshotID != snap || created.Filesystem != nil {
		t.Errorf("create request = %+v, want snapshot_id %s and no filesystem", created, snap)
	}

	// --fs names others: the flags' list goes beside snapshot_id and
	// replaces the snapshot's folders, as the help says.
	if code, _, stderr = runSandboxCLI(t, "up", "--fs", "fx=./fixtures"); code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "· fs 1 folder\n") || strings.Contains(stderr, "fs from snapshot") {
		t.Errorf("stderr:\n%s", stderr)
	}
	want := []api.FilesystemFolder{{Name: "fx", Upload: "up_1"}}
	if created := plane.createdReq(); created == nil || created.SnapshotID == nil || !reflect.DeepEqual(created.Filesystem, want) {
		t.Errorf("create request = %+v, want snapshot_id and %+v", created, want)
	}
}

func TestUpRefusesLocalFolderWhenThePlaneHasNoUploadsRoute(t *testing.T) {
	plane := newSandboxPlane(t)
	b := sandboxBench(t, plane.srv.URL)
	fsProject(t, b)
	plane.script(func(p *sandboxPlane) { p.uploadsStatus = 404 })

	code, _, stderr := runSandboxCLI(t, "up")
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, stderr)
	}
	sbInOrder(t, stderr, "✗ This control plane has no uploads route, so the local folder app (app) cannot be sent; name a git source instead (or a gs:// prefix in the config), or upgrade the control plane")
	if plane.createdReq() != nil {
		t.Error("a sandbox was created although a folder could not be sent")
	}
	if sbPointer(t, b) != nil {
		t.Error("the folder points at a sandbox that was never created")
	}
}

func TestUpWarnsWhenThePlaneIgnoredTheFilesystem(t *testing.T) {
	plane := newSandboxPlane(t)
	twins := newSandboxTwins(t)
	b := sandboxBench(t, plane.srv.URL)
	fsProject(t, b)
	// Ready, every twin up, and no filesystem member: an older plane.
	readyWith(plane, twins.services(false))

	code, _, stderr := runSandboxCLI(t, "up", "--fs", "https://github.com/acme/lib")
	if code != 0 {
		t.Fatalf("exit %d, want 0 (the sandbox is usable):\n%s", code, stderr)
	}
	sbInOrder(t, stderr,
		"! The control plane ignored the filesystem request (it predates agent filesystems); sandbox "+sbID+" has no folders\n",
		"  stripe     STRIPE_API_BASE=",
		"✓ Up: "+sbID)
}

// A filesystem member that carries an env hint (a pod path) still prints
// as WebDAV: the hint is not a host value, and nothing here hands it to a
// command.
func TestUpPrintsTheFilesystemAsWebDAVNotAsAHint(t *testing.T) {
	plane := newSandboxPlane(t)
	twins := newSandboxTwins(t)
	engine := newFSEngine(t)
	b := sandboxBench(t, plane.srv.URL)
	b.twoEnvs()
	svc := engine.service()
	svc.EnvHint = "WORKSPACE_DIR"
	readyWith(plane, append(twins.services(false), svc))

	code, _, stderr := runSandboxCLI(t, "up", "ci", "--fs", "https://github.com/acme/lib")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	sbInOrder(t, stderr, "· fs 1 folder\n", "  filesystem   WebDAV "+svc.URL)
	if strings.Contains(stderr, "WORKSPACE_DIR=http") {
		t.Errorf("the hint was printed as a host value:\n%s", stderr)
	}

	// Nor is it exported or handed over.
	code, stdout, stderr := runSandboxCLI(t, "sandbox", "exports")
	if code != 0 || strings.Contains(stdout, "WORKSPACE_DIR") || strings.Contains(stderr, "filesystem has no env hint") {
		t.Errorf("exports: exit %d\n%s\n%s", code, stdout, stderr)
	}
	if handed := handoffs([]api.ServiceInfo{svc}, nil, nil); len(handed) != 0 {
		t.Errorf("handoffs = %+v, want none for the filesystem", handed)
	}
	if code, _, stderr = runSandboxCLI(t, "up", "ci", "--fs", "https://github.com/acme/lib", "--fs", "https://github.com/acme/lib2"); code != 0 || !strings.Contains(stderr, "· fs 2 folders\n") {
		t.Errorf("exit %d:\n%s", code, stderr)
	}
}
