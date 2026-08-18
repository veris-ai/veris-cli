package bundlescan

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseVolumeVariants(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "bundle.pem")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		spec     string
		source   string
		dest     string
		hostDir  bool
		hostFile bool
	}{
		{dir + ":/app", dir, "/app", true, false},
		{dir + ":/app:ro", dir, "/app", true, false},
		{dir + ":/app/", dir, "/app", true, false}, // trailing slash normalised
		{file + ":/app/bundle.pem:ro", file, "/app/bundle.pem", false, true},
		{"named-vol:/data", "named-vol", "/data", false, false},
		{"/does/not/exist:/x", "/does/not/exist", "/x", false, false},
		{"/anon-dest", "", "/anon-dest", false, false},
	}
	for _, c := range cases {
		m := ParseVolume(c.spec)
		if m.Source != c.source || m.Dest != c.dest ||
			m.HostDir != c.hostDir || m.HostFile != c.hostFile {
			t.Errorf("ParseVolume(%q) = {Source:%q Dest:%q HostDir:%v HostFile:%v}, "+
				"want {%q %q %v %v}",
				c.spec, m.Source, m.Dest, m.HostDir, m.HostFile,
				c.source, c.dest, c.hostDir, c.hostFile)
		}
	}
}

func TestScanVolumeFindsBundlesUnderTheMountDestination(t *testing.T) {
	ca := testCA(t, "Vol Root")
	src := t.TempDir()
	rel := "venv/lib/python3.12/site-packages/certifi"
	if err := os.MkdirAll(filepath.Join(src, rel), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, rel, "cacert.pem"), ca, 0o644); err != nil {
		t.Fatal(err)
	}
	// A bare bundle name outside the table stays untouched.
	if err := os.WriteFile(filepath.Join(src, "cacert.pem"), ca, 0o644); err != nil {
		t.Fatal(err)
	}

	cands, _, err := ScanVolume(ParseVolume(src + ":/work:ro"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(cands), cands)
	}
	want := "/work/" + rel + "/cacert.pem"
	if cands[0].ContainerPath != want {
		t.Errorf("container path %q, want %q", cands[0].ContainerPath, want)
	}
	if !bytes.Equal(cands[0].Content, ca) {
		t.Error("the volume copy's bytes must be carried on the candidate")
	}
}

func TestScanVolumeMatchesTheDestinationAnchoredPath(t *testing.T) {
	// The mount DESTINATION supplies the anchoring SDK directory: the source
	// holds a bare cacert.pem, and only the container-side path says it is
	// certifi's bundle.
	ca := testCA(t, "Dest Root")
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "cacert.pem"), ca, 0o644); err != nil {
		t.Fatal(err)
	}
	dest := "/usr/local/lib/python3.12/site-packages/certifi"

	cands, _, err := ScanVolume(ParseVolume(src + ":" + dest))
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(cands), cands)
	}
	if cands[0].ContainerPath != dest+"/cacert.pem" || cands[0].SDK != "certifi" {
		t.Errorf("got %q as %q, want %q as certifi",
			cands[0].ContainerPath, cands[0].SDK, dest+"/cacert.pem")
	}
	if !bytes.Equal(cands[0].Content, ca) {
		t.Error("the volume copy's bytes must be carried on the candidate")
	}
}

func TestScanVolumeFollowsASymlinkedSource(t *testing.T) {
	ca := testCA(t, "Link Root")
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "certifi"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "certifi", "cacert.pem"), ca, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(src, link); err != nil {
		t.Fatal(err)
	}

	m := ParseVolume(link + ":/app")
	if !m.HostDir {
		t.Fatal("a symlink to a directory is a directory bind; it must classify as HostDir")
	}
	cands, _, err := ScanVolume(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].ContainerPath != "/app/certifi/cacert.pem" {
		t.Fatalf("a symlinked source must be walked through its target, got %+v", cands)
	}
	if !bytes.Equal(cands[0].Content, ca) {
		t.Error("the target's bytes must be carried on the candidate")
	}
}

func TestScanVolumeFollowsAnInTreeDirectorySymlink(t *testing.T) {
	// site-packages/certifi is a relative symlink to a sibling inside the
	// same mount -- the container resolves it, so the bundle behind it is
	// really readable at the symlink's own path.
	ca := testCA(t, "Linked Dir Root")
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "real-certifi"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "real-certifi", "cacert.pem"), ca, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "site-packages"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../real-certifi", filepath.Join(src, "site-packages", "certifi")); err != nil {
		t.Fatal(err)
	}

	cands, _, err := ScanVolume(ParseVolume(src + ":/app"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].ContainerPath != "/app/site-packages/certifi/cacert.pem" {
		t.Fatalf("the bundle behind an in-tree dir symlink must be found at the "+
			"symlink's path, got %+v", cands)
	}
	if !bytes.Equal(cands[0].Content, ca) {
		t.Error("the target's bytes must be carried on the candidate")
	}
}

func TestScanVolumeTerminatesADirectorySymlinkLoop(t *testing.T) {
	ca := testCA(t, "Loop Root")
	src := t.TempDir()
	for _, d := range []string{"a", "b", filepath.Join("b", "certifi")} {
		if err := os.MkdirAll(filepath.Join(src, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(src, "b", "certifi", "cacert.pem"), ca, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../b", filepath.Join(src, "a", "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../a", filepath.Join(src, "b", "link")); err != nil {
		t.Fatal(err)
	}

	// The pin is termination: an unvisited-set walk recurses a<->b forever.
	cands, _, err := ScanVolume(ParseVolume(src + ":/app"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, c := range cands {
		got[c.ContainerPath] = true
	}
	if !got["/app/b/certifi/cacert.pem"] || !got["/app/a/link/certifi/cacert.pem"] {
		t.Fatalf("both the direct and the linked path must be found, got %+v", cands)
	}
}

func TestScanVolumeIgnoresAnEscapingDirectorySymlink(t *testing.T) {
	// A relative link whose target sits OUTSIDE the mount: the bind exposes
	// nothing behind it in the container, so following it would patch a
	// phantom path.
	ca := testCA(t, "Outside Root")
	parent := t.TempDir()
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(filepath.Join(outside, "certifi"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "certifi", "cacert.pem"), ca, 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(parent, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../outside", filepath.Join(src, "pkgs")); err != nil {
		t.Fatal(err)
	}

	cands, _, err := ScanVolume(ParseVolume(src + ":/app"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("a dir symlink escaping the mount must not be followed, got %+v", cands)
	}
}

func TestScanVolumeWalksEachAliasOfASharedTarget(t *testing.T) {
	// Two sibling symlinks to ONE real directory are two distinct
	// container-visible paths; a cycle guard that remembers the target
	// globally would scan the first alias and silently skip the second.
	ca := testCA(t, "Shared Root")
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "real", "cacert.pem"), ca, 0o644); err != nil {
		t.Fatal(err)
	}
	// Both alias names anchor a table rule, and botocore sorts before
	// certifi, so the second alias is exactly the one a global guard loses.
	for _, alias := range []string{"botocore", "certifi"} {
		if err := os.Symlink("real", filepath.Join(src, alias)); err != nil {
			t.Fatal(err)
		}
	}

	cands, _, err := ScanVolume(ParseVolume(src + ":/app"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, c := range cands {
		got[c.ContainerPath] = true
	}
	if len(cands) != 2 || !got["/app/botocore/cacert.pem"] || !got["/app/certifi/cacert.pem"] {
		t.Fatalf("every alias of a shared target must yield its own candidate, got %+v", cands)
	}
}

func TestScanVolumeDoesNotFollowAnAbsoluteDirectorySymlink(t *testing.T) {
	// The host target sits inside the mount, but the CONTAINER resolves an
	// absolute link against its own root -- what it finds there is not what
	// the host-side walk would have read.
	ca := testCA(t, "Absolute Root")
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "real-certifi"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "real-certifi", "cacert.pem"), ca, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(src, "real-certifi"), filepath.Join(src, "certifi")); err != nil {
		t.Fatal(err)
	}

	cands, _, err := ScanVolume(ParseVolume(src + ":/app"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("an absolute dir symlink must not be followed even when its host "+
			"target is in-root, got %+v", cands)
	}
}

func TestScanVolumeAbortsOnItsFileBudget(t *testing.T) {
	src := t.TempDir()
	for i := 0; i < 8; i++ {
		if err := os.WriteFile(filepath.Join(src, fmt.Sprintf("f%d", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := maxWalkFiles
	maxWalkFiles = 4
	defer func() { maxWalkFiles = old }()

	_, _, err := ScanVolume(ParseVolume(src + ":/work"))
	if err == nil || !strings.Contains(err.Error(), "file budget") {
		t.Fatalf("a walk past its file budget must abort loudly, got %v", err)
	}
}

func TestScanVolumeStaysAboveItsDepthBound(t *testing.T) {
	ca := testCA(t, "Deep Root")
	src := t.TempDir()
	deep := src
	for i := 0; i < maxWalkDepth; i++ {
		deep = filepath.Join(deep, fmt.Sprintf("d%d", i))
	}
	if err := os.MkdirAll(filepath.Join(deep, "certifi"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "certifi", "cacert.pem"), ca, 0o644); err != nil {
		t.Fatal(err)
	}
	cands, _, err := ScanVolume(ParseVolume(src + ":/work"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("a bundle below the depth bound is out of scope by design, got %+v", cands)
	}
}

func TestCollectPrefersTheVolumeCopyOverTheImage(t *testing.T) {
	imageCA := testCA(t, "Image Root")
	volCA := testCA(t, "Volume Root")

	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "certifi"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "certifi", "cacert.pem"), volCA, 0o644); err != nil {
		t.Fatal(err)
	}

	fake := &fakeDocker{exportTar: buildTar(t, []tarEntry{
		// Shadowed by the -v below: the volume copy is the effective one.
		{name: "app/certifi/cacert.pem", typ: tar.TypeReg, body: imageCA},
		// Untouched by any mount, so it survives.
		{name: certifiPath, typ: tar.TypeReg, body: imageCA},
	})}
	s := &Scanner{Docker: fake}

	cands, _, err := s.Collect(context.Background(), "img:latest", []string{src + ":/app"})
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string][]byte{}
	for _, c := range cands {
		byPath[c.ContainerPath] = c.Content
	}
	if len(cands) != 2 {
		t.Fatalf("got %d candidates, want 2: %+v", len(cands), cands)
	}
	if !bytes.Equal(byPath["/app/certifi/cacert.pem"], volCA) {
		t.Error("the covered path must carry the VOLUME copy's bytes")
	}
	if !bytes.Equal(byPath["/"+certifiPath], imageCA) {
		t.Error("the uncovered image match must survive")
	}
}

func TestCollectGovernsBindCandidatesByTheDeepestMount(t *testing.T) {
	outerCA := testCA(t, "Outer Root")
	deeperCA := testCA(t, "Deeper Root")

	outer := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outer, "certifi"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outer, "certifi", "cacert.pem"), outerCA, 0o644); err != nil {
		t.Fatal(err)
	}
	deeper := t.TempDir()
	if err := os.WriteFile(filepath.Join(deeper, "cacert.pem"), deeperCA, 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Scanner{Docker: &fakeDocker{exportTar: buildTar(t, nil)}}
	cands, _, err := s.Collect(context.Background(), "img:latest",
		[]string{outer + ":/app", deeper + ":/app/certifi"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want only the deeper mount's: %+v", len(cands), cands)
	}
	if !bytes.Equal(cands[0].Content, deeperCA) {
		t.Error("the deeper mount masks the outer bind, so its bytes are the effective ones")
	}

	// A deeper mount WITHOUT the file masks the outer copy entirely: the path
	// does not exist in the container, so nothing may be over-mounted there.
	s = &Scanner{Docker: &fakeDocker{exportTar: buildTar(t, nil)}}
	cands, _, err = s.Collect(context.Background(), "img:latest",
		[]string{outer + ":/app", t.TempDir() + ":/app/certifi"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("an outer bind's copy under a deeper empty mount is masked, got %+v", cands)
	}
}

func TestCollectRefusesADeeperUnscannableMountOverABindCandidate(t *testing.T) {
	ca := testCA(t, "Outer Root")
	outer := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outer, "certifi"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outer, "certifi", "cacert.pem"), ca, 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Scanner{Docker: &fakeDocker{exportTar: buildTar(t, nil)}}
	_, _, err := s.Collect(context.Background(), "img:latest",
		[]string{outer + ":/app", "named-vol:/app/certifi"})
	if err == nil || !strings.Contains(err.Error(), "cannot see inside") {
		t.Fatalf("a named volume over a bind's bundle must refuse, got %v", err)
	}
}

func TestCollectKeepsAnImageCandidateUnderAnAnonymousVolume(t *testing.T) {
	ca := testCA(t, "Image Root")
	fake := &fakeDocker{exportTar: buildTar(t, []tarEntry{
		{name: "app/certifi/cacert.pem", typ: tar.TypeReg, body: ca},
	})}
	s := &Scanner{Docker: fake}

	// An anonymous volume is copied up from the image, so the image scan
	// already saw the effective bytes and the deeper overlay composes.
	cands, _, err := s.Collect(context.Background(), "img:latest", []string{"/app"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].ContainerPath != "/app/certifi/cacert.pem" {
		t.Fatalf("the image candidate under an anonymous volume must survive, got %+v", cands)
	}
	if !bytes.Equal(cands[0].Content, ca) {
		t.Error("the image copy's bytes are the effective ones under copy-up")
	}
}

func TestCollectRefusesAnExactMountOverACandidate(t *testing.T) {
	// A DIRECTORY bound at the exact bundle path: unlike a file bind, its
	// contents cannot be the bundle, so the conflict refuses.
	ca := testCA(t, "Root")
	fake := &fakeDocker{exportTar: buildTar(t, []tarEntry{
		{name: certifiPath, typ: tar.TypeReg, body: ca},
	})}
	s := &Scanner{Docker: fake}

	_, _, err := s.Collect(context.Background(), "img:latest",
		[]string{t.TempDir() + ":/" + certifiPath + ":ro"})
	if err == nil || !strings.Contains(err.Error(), "already mounts that exact path") {
		t.Fatalf("an exact non-file mount over a candidate must refuse, got %v", err)
	}
}

func TestCollectPatchesAHostFileMountedAtABundlePath(t *testing.T) {
	imageCA := testCA(t, "Image Root")
	fileCA := testCA(t, "File Root")
	file := filepath.Join(t.TempDir(), "own.pem")
	if err := os.WriteFile(file, fileCA, 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &fakeDocker{exportTar: buildTar(t, []tarEntry{
		{name: certifiPath, typ: tar.TypeReg, body: imageCA},
	})}
	s := &Scanner{Docker: fake}

	cands, _, err := s.Collect(context.Background(), "img:latest",
		[]string{file + ":/" + certifiPath + ":ro"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].ContainerPath != "/"+certifiPath {
		t.Fatalf("a file bound at a bundle path must be the candidate there, got %+v", cands)
	}
	if !bytes.Equal(cands[0].Content, fileCA) {
		t.Error("the file bind masks the image copy, so its bytes are the effective ones")
	}
	overlays, skipped, err := WriteOverlays(t.TempDir(), testCA(t, "Veris Local CA"), cands)
	if err != nil {
		t.Fatal(err)
	}
	if len(overlays) != 1 || len(skipped) != 0 ||
		overlays[0].ContainerPath != "/"+certifiPath {
		t.Fatalf("the file bind's candidate must get an overlay, got %+v", overlays)
	}
}

func TestCollectRefusesAJunkHostFileAtABundlePath(t *testing.T) {
	file := filepath.Join(t.TempDir(), "own.pem")
	if err := os.WriteFile(file, []byte("not a bundle at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Scanner{Docker: &fakeDocker{exportTar: buildTar(t, nil)}}

	_, _, err := s.Collect(context.Background(), "img:latest",
		[]string{file + ":/" + certifiPath + ":ro"})
	if err == nil || !strings.Contains(err.Error(), "not a CA bundle") {
		t.Fatalf("junk bound at a bundle path must refuse loudly, got %v", err)
	}
}

func TestCollectRefusesAnUnscannableMountShadowingABundle(t *testing.T) {
	ca := testCA(t, "Root")
	fake := &fakeDocker{exportTar: buildTar(t, []tarEntry{
		{name: "app/certifi/cacert.pem", typ: tar.TypeReg, body: ca},
	})}
	s := &Scanner{Docker: fake}

	_, _, err := s.Collect(context.Background(), "img:latest", []string{"named-vol:/app"})
	if err == nil || !strings.Contains(err.Error(), "cannot see inside") {
		t.Fatalf("a named volume shadowing a bundle must refuse, got %v", err)
	}
}

func writeFile(t *testing.T, root, rel string, body []byte) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// A mounted tree can carry the unknown bundle too -- a vendored trust file in
// a bind-mounted venv or node_modules -- and the walk names it by the path
// the CONTAINER sees.
func TestVolumeWalkReportsUnknownCandidates(t *testing.T) {
	ca := testCA(t, "Root W")
	src := t.TempDir()
	writeFile(t, src, "vendor/trust/cacert.pem", ca)
	writeFile(t, src, "vendor/fixtures/ca-bundle.crt", []byte("fixture, not a bundle"))

	_, unknown, err := ScanVolume(ParseVolume(src + ":/app"))
	if err != nil {
		t.Fatal(err)
	}
	if len(unknown) != 1 || unknown[0] != "/app/vendor/trust/cacert.pem" {
		t.Fatalf("unknown = %v, want the validated off-table bundle at its container path", unknown)
	}
}

// The report is capped and ordered: SDK-looking paths ahead of system trust
// stores, because when an SDK refused the certificate its own vendored bundle
// is the likelier read.
func TestCapUnknownOrdersSDKPathsFirst(t *testing.T) {
	got := capUnknown([]string{
		"/etc/ssl/certs/ca-certificates.crt",
		"/opt/venv/lib/app/vendor/cacert.pem",
		"/etc/ssl/certs/ca-certificates.crt", // duplicate
	})
	want := []string{"/opt/venv/lib/app/vendor/cacert.pem", "/etc/ssl/certs/ca-certificates.crt"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("capUnknown = %v, want %v", got, want)
	}
}
