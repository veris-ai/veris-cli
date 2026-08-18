package bundlescan

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"path"
	"strings"
	"testing"
	"time"
)

// fakeDocker plays the daemon: a fixed image ID, a fixed created container, a
// canned export tar, and per-path docker-cp answers.
type fakeDocker struct {
	exportTar   []byte
	cpFiles     map[string][]byte // container-absolute path -> bytes
	stuckExport bool              // export never yields data until ctx dies

	calls       []string
	exportCalls int
}

func (f *fakeDocker) Output(args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	switch args[0] {
	case "image":
		return []byte("sha256:feedface\n"), nil
	case "create":
		return []byte("ctr-test\n"), nil
	case "rm":
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected docker %v", args)
}

func (f *fakeDocker) Stream(ctx context.Context, args ...string) (io.ReadCloser, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	switch args[0] {
	case "export":
		f.exportCalls++
		if f.stuckExport {
			return stuckStream{ctx: ctx}, nil
		}
		return io.NopCloser(bytes.NewReader(f.exportTar)), nil
	case "cp":
		ref := args[2] // ctr:<path>
		p := ref[strings.Index(ref, ":")+1:]
		content, ok := f.cpFiles[p]
		if !ok {
			return io.NopCloser(bytes.NewReader(nil)), nil
		}
		return io.NopCloser(bytes.NewReader(singleFileTar(p, content))), nil
	}
	return nil, fmt.Errorf("unexpected docker %v", args)
}

// stuckStream blocks every read until the scan's budget kills the context,
// which is what a wedged daemon looks like from this side of the pipe.
type stuckStream struct{ ctx context.Context }

func (s stuckStream) Read([]byte) (int, error) {
	<-s.ctx.Done()
	return 0, s.ctx.Err()
}
func (s stuckStream) Close() error { return nil }

// singleFileTar is the shape `docker cp <ctr>:<path> -` writes.
func singleFileTar(name string, content []byte) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{
		Name: path.Base(name), Typeflag: tar.TypeReg,
		Mode: 0o644, Size: int64(len(content)),
	})
	_, _ = tw.Write(content)
	_ = tw.Close()
	return buf.Bytes()
}

func (f *fakeDocker) called(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

const certifiPath = "usr/lib/python3.12/site-packages/certifi/cacert.pem"

func TestScanExportTarExtractsMatchesInOnePass(t *testing.T) {
	ca := testCA(t, "Root A")
	tarBytes := buildTar(t, []tarEntry{
		{name: "usr", typ: tar.TypeDir},
		{name: certifiPath, typ: tar.TypeReg, body: ca},
		{name: "app/testdata/cacert.pem", typ: tar.TypeReg, body: ca}, // off-table
		{name: "etc/hostname", typ: tar.TypeReg, body: []byte("box")},
	})
	matches, _, err := scanExportTar(bytes.NewReader(tarBytes))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want exactly the certifi one: %+v", len(matches), matches)
	}
	if matches[0].path != certifiPath || matches[0].rule.SDK != "certifi" {
		t.Errorf("matched %q as %q", matches[0].path, matches[0].rule.SDK)
	}
	if !bytes.Equal(matches[0].content, ca) {
		t.Error("the content must be extracted during the same pass")
	}
}

func TestScanExportTarResolvesLinksInsideTheArchive(t *testing.T) {
	ca := testCA(t, "Root B")
	tarBytes := buildTar(t, []tarEntry{
		// The target streams past BEFORE one link and AFTER the other: a
		// single pass has to hold the bytes either way.
		{name: "opt/venv/lib/python3.11/site-packages/botocore/cacert.pem",
			typ: tar.TypeSymlink, link: "../../../../../../etc/ssl/certs/ca-certificates.crt"},
		{name: "etc/ssl/certs/ca-certificates.crt", typ: tar.TypeReg, body: ca},
		{name: certifiPath, typ: tar.TypeSymlink, link: "/etc/ssl/certs/ca-certificates.crt"},
		{name: "usr/lib/python3/dist-packages/httplib2/cacerts.txt",
			typ: tar.TypeLink, link: "etc/ssl/certs/ca-certificates.crt"},
	})
	matches, _, err := scanExportTar(bytes.NewReader(tarBytes))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 3 {
		t.Fatalf("got %d matches, want 3: %+v", len(matches), matches)
	}
	for _, m := range matches {
		if !bytes.Equal(m.content, ca) {
			t.Errorf("%s at %s: link did not resolve to the target's bytes", m.rule.SDK, m.path)
		}
	}
}

func TestScanExportTarRejectsAnEscapingSymlink(t *testing.T) {
	tarBytes := buildTar(t, []tarEntry{
		{name: "pkg/certifi/cacert.pem", typ: tar.TypeSymlink, link: "../../../outside.pem"},
	})
	_, _, err := scanExportTar(bytes.NewReader(tarBytes))
	if err == nil || !strings.Contains(err.Error(), "outside the image root") {
		t.Fatalf("an escaping symlink must be rejected, got %v", err)
	}
}

func TestScanExportTarBoundsLinkHops(t *testing.T) {
	entries := []tarEntry{{name: certifiPath, typ: tar.TypeSymlink, link: "/hop0"}}
	for i := 0; i <= maxLinkHops; i++ {
		entries = append(entries, tarEntry{
			name: fmt.Sprintf("hop%d", i), typ: tar.TypeSymlink,
			link: fmt.Sprintf("/hop%d", i+1),
		})
	}
	_, _, err := scanExportTar(bytes.NewReader(buildTar(t, entries)))
	if err == nil || !strings.Contains(err.Error(), "link hops") {
		t.Fatalf("a link chain past the hop limit must be rejected, got %v", err)
	}
}

func TestScanExportTarRejectsAnOversizedBundle(t *testing.T) {
	tarBytes := buildTar(t, []tarEntry{
		{name: certifiPath, typ: tar.TypeReg, body: bytes.Repeat([]byte("A"), maxBundleSize+1)},
	})
	_, _, err := scanExportTar(bytes.NewReader(tarBytes))
	if err == nil || !strings.Contains(err.Error(), "larger than any CA bundle") {
		t.Fatalf("an oversized match must be rejected, got %v", err)
	}
}

func TestScanImageAbortsOnAMatchThatDoesNotValidate(t *testing.T) {
	fake := &fakeDocker{exportTar: buildTar(t, []tarEntry{
		{name: certifiPath, typ: tar.TypeReg, body: []byte("not a bundle")},
	})}
	s := &Scanner{Docker: fake}
	_, _, err := s.ScanImage(context.Background(), "img:latest")
	if err == nil || !strings.Contains(err.Error(), certifiPath) {
		t.Fatalf("a known candidate that fails validation must abort naming the file, got %v", err)
	}
	if !fake.called("rm -f ctr-test") {
		t.Error("the created container must be removed on the error path")
	}
}

func TestScanImageCachesMatchedPathsByImageID(t *testing.T) {
	ca := testCA(t, "Root C")
	fake := &fakeDocker{
		exportTar: buildTar(t, []tarEntry{{name: certifiPath, typ: tar.TypeReg, body: ca}}),
		cpFiles:   map[string][]byte{"/" + certifiPath: ca},
	}
	s := &Scanner{Docker: fake, CacheDir: t.TempDir()}

	first, _, err := s.ScanImage(context.Background(), "img:latest")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := s.ScanImage(context.Background(), "img:latest")
	if err != nil {
		t.Fatal(err)
	}
	if fake.exportCalls != 1 {
		t.Errorf("the second scan ran %d exports; the cache should have spared it", fake.exportCalls)
	}
	if !fake.called("cp -L") {
		t.Error("a cache hit must re-extract contents via docker cp")
	}
	if len(first) != 1 || len(second) != 1 ||
		first[0].ContainerPath != second[0].ContainerPath ||
		!bytes.Equal(first[0].Content, second[0].Content) {
		t.Errorf("cache hit and miss must agree: %+v vs %+v", first, second)
	}
}

func TestScanImageCacheMissesWhenTheRuleTableChanges(t *testing.T) {
	ca := testCA(t, "Root E")
	fake := &fakeDocker{
		exportTar: buildTar(t, []tarEntry{{name: certifiPath, typ: tar.TypeReg, body: ca}}),
		cpFiles:   map[string][]byte{"/" + certifiPath: ca},
	}
	s := &Scanner{Docker: fake, CacheDir: t.TempDir()}

	if _, _, err := s.ScanImage(context.Background(), "img:latest"); err != nil {
		t.Fatal(err)
	}

	// A rule landing after the entry was written changes what the scan would
	// match, so the entry must read as a miss and the image be re-exported.
	old := rules
	rules = append(append([]rule{}, rules...), rule{"future-sdk", "future-sdk/cacert.pem"})
	defer func() { rules = old }()

	if _, _, err := s.ScanImage(context.Background(), "img:latest"); err != nil {
		t.Fatal(err)
	}
	if fake.exportCalls != 2 {
		t.Fatalf("a cache entry from an older rule table must not pin its match set; "+
			"got %d export(s), want 2", fake.exportCalls)
	}
}

func TestScanImageBudgetSparesACompletedRead(t *testing.T) {
	ca := testCA(t, "Root F")
	fake := &fakeDocker{
		exportTar: buildTar(t, []tarEntry{{name: certifiPath, typ: tar.TypeReg, body: ca}}),
	}
	// A deadline this short has fired long before the verdict is read; only a
	// scan that actually ended early may wear the budget error.
	s := &Scanner{Docker: fake, Budget: time.Nanosecond}
	cands, _, err := s.ScanImage(context.Background(), "img:latest")
	if err != nil {
		t.Fatalf("a fully read export must never be reported as not fully read: %v", err)
	}
	if len(cands) != 1 || !bytes.Equal(cands[0].Content, ca) {
		t.Fatalf("the completed scan's candidates must survive: %+v", cands)
	}
}

func TestScanImageBudgetAbortsLoudly(t *testing.T) {
	fake := &fakeDocker{stuckExport: true}
	s := &Scanner{Docker: fake, Budget: 30 * time.Millisecond}
	_, _, err := s.ScanImage(context.Background(), "img:latest")
	if err == nil || !strings.Contains(err.Error(), "exceeded") ||
		!strings.Contains(err.Error(), "budget") {
		t.Fatalf("a scan past its budget must abort loudly, got %v", err)
	}
	if !fake.called("rm -f ctr-test") {
		t.Error("the created container must be removed when the budget expires")
	}
}

func TestScanImageFetchesLinkTargetsOutsideTheStash(t *testing.T) {
	ca := testCA(t, "Root D")
	// trust-anchors.pem is not a basename the pass stashes (no bundle marker
	// in the name), so the content has to come back through the targeted
	// docker cp.
	fake := &fakeDocker{
		exportTar: buildTar(t, []tarEntry{
			{name: certifiPath, typ: tar.TypeSymlink,
				link: "/etc/pki/ca-trust/extracted/pem/trust-anchors.pem"},
			{name: "etc/pki/ca-trust/extracted/pem/trust-anchors.pem",
				typ: tar.TypeReg, body: ca},
		}),
		cpFiles: map[string][]byte{"/" + certifiPath: ca},
	}
	s := &Scanner{Docker: fake}
	cands, _, err := s.ScanImage(context.Background(), "img:latest")
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || !bytes.Equal(cands[0].Content, ca) {
		t.Fatalf("the cp fallback must recover the target's bytes: %+v", cands)
	}
	if !fake.called("cp -L") {
		t.Error("an off-stash link target must be fetched with docker cp -L")
	}
}

// The unknown-candidate channel: bundle-shaped files OUTSIDE the rule table
// are validated and reported by path, never patched. A known match stays out
// of the report, and a lookalike that is not a real CA bundle is dropped --
// the report exists to hand a refusal diagnostic the one file worth
// over-mounting by hand.
func TestScanImageReportsUnknownCandidates(t *testing.T) {
	ca := testCA(t, "Root U")
	fake := &fakeDocker{exportTar: buildTar(t, []tarEntry{
		{name: certifiPath, typ: tar.TypeReg, body: ca},                                        // known rule
		{name: "opt/trust/cacert.pem", typ: tar.TypeReg, body: ca},                             // unknown, real
		{name: "app/fixtures/ca-bundle.crt", typ: tar.TypeReg, body: []byte("just a fixture")}, // lookalike
		{name: "etc/hostname", typ: tar.TypeReg, body: []byte("box")},                          // not bundle-shaped
	})}
	s := &Scanner{Docker: fake}
	cands, unknown, err := s.ScanImage(context.Background(), "img:latest")
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].ContainerPath != "/"+certifiPath {
		t.Fatalf("cands = %+v, want exactly the certifi match", cands)
	}
	if len(unknown) != 1 || unknown[0] != "/opt/trust/cacert.pem" {
		t.Fatalf("unknown = %v, want exactly the validated off-table bundle", unknown)
	}
}

// Unknown candidates ride the cache: a cache-hit run still has them to
// report after a refusal, without re-exporting the image.
func TestUnknownCandidatesSurviveTheCache(t *testing.T) {
	ca := testCA(t, "Root V")
	fake := &fakeDocker{
		exportTar: buildTar(t, []tarEntry{
			{name: certifiPath, typ: tar.TypeReg, body: ca},
			{name: "opt/trust/cacert.pem", typ: tar.TypeReg, body: ca},
		}),
		cpFiles: map[string][]byte{"/" + certifiPath: ca},
	}
	s := &Scanner{Docker: fake, CacheDir: t.TempDir()}
	if _, _, err := s.ScanImage(context.Background(), "img:latest"); err != nil {
		t.Fatal(err)
	}
	_, unknown, err := s.ScanImage(context.Background(), "img:latest")
	if err != nil {
		t.Fatal(err)
	}
	if fake.exportCalls != 1 {
		t.Fatalf("the second scan ran %d exports; the cache should have spared it", fake.exportCalls)
	}
	if len(unknown) != 1 || unknown[0] != "/opt/trust/cacert.pem" {
		t.Fatalf("cached unknown = %v, want the off-table bundle", unknown)
	}
}
