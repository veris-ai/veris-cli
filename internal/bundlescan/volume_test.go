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
		spec    string
		source  string
		dest    string
		hostDir bool
	}{
		{dir + ":/app", dir, "/app", true},
		{dir + ":/app:ro", dir, "/app", true},
		{dir + ":/app/", dir, "/app", true}, // trailing slash normalised
		{file + ":/app/bundle.pem:ro", file, "/app/bundle.pem", false},
		{"named-vol:/data", "named-vol", "/data", false},
		{"/does/not/exist:/x", "/does/not/exist", "/x", false},
		{"/anon-dest", "", "/anon-dest", false},
	}
	for _, c := range cases {
		m := ParseVolume(c.spec)
		if m.Source != c.source || m.Dest != c.dest || m.HostDir != c.hostDir {
			t.Errorf("ParseVolume(%q) = {Source:%q Dest:%q HostDir:%v}, want {%q %q %v}",
				c.spec, m.Source, m.Dest, m.HostDir, c.source, c.dest, c.hostDir)
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

	cands, err := ScanVolume(ParseVolume(src + ":/work:ro"))
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

	cands, err := ScanVolume(ParseVolume(src + ":" + dest))
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
	cands, err := ScanVolume(m)
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

	_, err := ScanVolume(ParseVolume(src + ":/work"))
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
	cands, err := ScanVolume(ParseVolume(src + ":/work"))
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

	cands, err := s.Collect(context.Background(), "img:latest", []string{src + ":/app"})
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
	cands, err := s.Collect(context.Background(), "img:latest",
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
	cands, err = s.Collect(context.Background(), "img:latest",
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
	_, err := s.Collect(context.Background(), "img:latest",
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
	cands, err := s.Collect(context.Background(), "img:latest", []string{"/app"})
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
	ca := testCA(t, "Root")
	file := filepath.Join(t.TempDir(), "own.pem")
	if err := os.WriteFile(file, ca, 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &fakeDocker{exportTar: buildTar(t, []tarEntry{
		{name: certifiPath, typ: tar.TypeReg, body: ca},
	})}
	s := &Scanner{Docker: fake}

	_, err := s.Collect(context.Background(), "img:latest",
		[]string{file + ":/" + certifiPath + ":ro"})
	if err == nil || !strings.Contains(err.Error(), "already mounts that exact path") {
		t.Fatalf("an exact user mount over a candidate must refuse, got %v", err)
	}
}

func TestCollectRefusesAnUnscannableMountShadowingABundle(t *testing.T) {
	ca := testCA(t, "Root")
	fake := &fakeDocker{exportTar: buildTar(t, []tarEntry{
		{name: "app/certifi/cacert.pem", typ: tar.TypeReg, body: ca},
	})}
	s := &Scanner{Docker: fake}

	_, err := s.Collect(context.Background(), "img:latest", []string{"named-vol:/app"})
	if err == nil || !strings.Contains(err.Error(), "cannot see inside") {
		t.Fatalf("a named volume shadowing a bundle must refuse, got %v", err)
	}
}
