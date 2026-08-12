package bundlescan

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Docker is the execution seam: everything the scan asks of the docker CLI
// goes through it, so unit tests hand back synthetic tars instead of needing
// a daemon.
type Docker interface {
	// Output runs `docker args...` and returns its stdout.
	Output(args ...string) ([]byte, error)
	// Stream runs `docker args...` with stdout as a stream. Cancelling ctx
	// kills the command, which is how the scan budget interrupts an export
	// mid-flight. Close reaps the command and reports its exit.
	Stream(ctx context.Context, args ...string) (io.ReadCloser, error)
}

// CLI is the real docker client.
type CLI struct{}

func (CLI) Output(args ...string) ([]byte, error) {
	out, err := exec.Command("docker", args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("docker %s: %w: %s",
				strings.Join(args, " "), err, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

func (CLI) Stream(ctx context.Context, args ...string) (io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = pipe.Close()
		return nil, fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
	}
	return &cmdStream{pipe: pipe, cmd: cmd, args: args, stderr: &stderr}, nil
}

type cmdStream struct {
	pipe   io.ReadCloser
	cmd    *exec.Cmd
	args   []string
	stderr *bytes.Buffer

	once     sync.Once
	closeErr error
}

func (s *cmdStream) Read(p []byte) (int, error) { return s.pipe.Read(p) }

// Close reaps the command and reports how it exited -- a stream that ended
// early looks like a clean, shorter tar, so the exit status is the only place
// a failed export shows up. Idempotent, so error paths may close eagerly.
func (s *cmdStream) Close() error {
	s.once.Do(func() {
		_ = s.pipe.Close()
		if err := s.cmd.Wait(); err != nil {
			s.closeErr = fmt.Errorf("docker %s: %w: %s",
				strings.Join(s.args, " "), err, strings.TrimSpace(s.stderr.String()))
		}
	})
	return s.closeErr
}

// DefaultBudget bounds the export scan. Streaming even a multi-gigabyte image
// out of a local daemon finishes well inside a minute; a scan that cannot
// finish in two is stuck, and the flag's posture is to say so rather than run
// partially patched.
const DefaultBudget = 2 * time.Minute

// ContainerLabelKey labels the throwaway containers the scan creates, so a
// teardown that outlives an interrupted scan can find and remove them.
const ContainerLabelKey = "veris-bundlescan"

// Scanner finds bundled CA files in a workload image.
type Scanner struct {
	// Docker defaults to the real CLI.
	Docker Docker
	// CacheDir holds scan results keyed by image ID and rule table; empty
	// disables caching.
	CacheDir string
	// Budget bounds the export scan; zero means DefaultBudget.
	Budget time.Duration
	// ContainerLabel, when set, labels every container the scan creates as
	// ContainerLabelKey=<value>. A signal between create and the deferred
	// remove strands the container; the label is what a later reap filters on.
	ContainerLabel string
}

func (s *Scanner) cli() Docker {
	if s.Docker == nil {
		return CLI{}
	}
	return s.Docker
}

func (s *Scanner) budget() time.Duration {
	if s.Budget <= 0 {
		return DefaultBudget
	}
	return s.Budget
}

// The cache records matched PATHS per immutable image ID, never contents:
// patching appends today's CA to the file's bytes, so contents are
// re-extracted every run anyway -- and with the paths known, extraction is a
// `docker cp` per file from a created container, which needs no full export.
// Discovery is the expensive half (streaming the image's whole filesystem),
// and it is the half an immutable ID can vouch for.
type cacheEntry struct {
	ImageID string `json:"image_id"`
	// Rules fingerprints the table the matches came from: an entry written
	// under an older table must read as a miss, or a rule added later would
	// never fire against a cached image.
	Rules   string        `json:"rules"`
	Matches []cachedMatch `json:"matches"`
}

type cachedMatch struct {
	SDK  string `json:"sdk"`
	Path string `json:"path"` // container-absolute
}

// ScanImage returns every validated bundled-CA file in the image. A matched
// path that cannot be extracted or validated is an error, not a skip: a known
// bundle left unpatched fails the workload later with everything here looking
// healthy.
func (s *Scanner) ScanImage(ctx context.Context, image string) ([]Candidate, error) {
	id, err := s.imageID(image)
	if err != nil {
		return nil, err
	}
	if cached, ok := s.readCache(id); ok {
		return s.extractCached(ctx, image, cached)
	}

	ctr, err := s.createContainer(image)
	if err != nil {
		return nil, err
	}
	defer s.removeContainer(ctr)

	budget := s.budget()
	sctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	rc, err := s.cli().Stream(sctx, "export", ctr)
	if err != nil {
		return nil, fmt.Errorf("bundle scan of %s: %w", image, err)
	}
	matches, scanErr := scanExportTar(rc)
	closeErr := rc.Close()
	// The budget verdict only when the scan actually ended early: a killed
	// export surfaces as a truncated tar or a failed exit, and either misread
	// would report "no bundles" for an image that was never fully scanned. A
	// tar parsed to its end from an export that exited clean was fully read,
	// however late the deadline fired.
	if scanErr != nil || closeErr != nil {
		if sctx.Err() != nil && ctx.Err() == nil {
			return nil, fmt.Errorf(
				"bundle scan of %s exceeded its %s budget: the image's filesystem was "+
					"not fully read, so a bundle in the unread remainder would ship "+
					"unpatched. Retry, or drop --patch-bundled-cas", image, budget)
		}
		if scanErr != nil {
			return nil, fmt.Errorf("bundle scan of %s: %w", image, scanErr)
		}
		return nil, fmt.Errorf("bundle scan of %s: %w", image, closeErr)
	}

	var cands []Candidate
	for _, m := range matches {
		content := m.content
		if content == nil {
			// The one-pass stash holds only bundle-shaped basenames, and a
			// symlink may point at any name. The created container is still
			// here, so fetch just that file; cp -L follows the link.
			content, err = s.copyOut(ctx, ctr, "/"+m.path)
			if err != nil {
				return nil, fmt.Errorf("%s bundle at /%s: %w", m.rule.SDK, m.path, err)
			}
		}
		if err := validate(content); err != nil {
			return nil, fmt.Errorf("%s bundle at /%s: %v", m.rule.SDK, m.path, err)
		}
		cands = append(cands, Candidate{
			SDK:           m.rule.SDK,
			ContainerPath: "/" + m.path,
			Content:       content,
			Origin:        "image " + image,
		})
	}
	s.writeCache(id, matches)
	return cands, nil
}

// extractCached is the cache-hit path: the paths are known, so each file is
// fetched individually from a created container instead of exporting the
// whole filesystem again.
func (s *Scanner) extractCached(ctx context.Context, image string, cached []cachedMatch) ([]Candidate, error) {
	if len(cached) == 0 {
		return nil, nil
	}
	ctr, err := s.createContainer(image)
	if err != nil {
		return nil, err
	}
	defer s.removeContainer(ctr)

	var cands []Candidate
	for _, m := range cached {
		content, err := s.copyOut(ctx, ctr, m.Path)
		if err != nil {
			return nil, fmt.Errorf("%s bundle at %s: %w", m.SDK, m.Path, err)
		}
		if err := validate(content); err != nil {
			return nil, fmt.Errorf("%s bundle at %s: %v", m.SDK, m.Path, err)
		}
		cands = append(cands, Candidate{
			SDK:           m.SDK,
			ContainerPath: m.Path,
			Content:       content,
			Origin:        "image " + image,
		})
	}
	return cands, nil
}

// imageID reads the image's immutable ID, which is what makes the cache safe:
// a tag can move, the ID cannot.
func (s *Scanner) imageID(image string) (string, error) {
	out, err := s.cli().Output("image", "inspect", "-f", "{{.Id}}", image)
	if err != nil {
		return "", fmt.Errorf("bundle scan cannot identify %s: %w", image, err)
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", fmt.Errorf("bundle scan cannot identify %s: inspect returned no id", image)
	}
	return id, nil
}

// createContainer makes a container from the image WITHOUT starting it:
// export and cp read the filesystem straight from the layers, so this works
// for images with no shell and never runs the workload early. The placeholder
// command is never executed; it only satisfies images that declare neither an
// ENTRYPOINT nor a CMD, which `docker create` otherwise refuses.
func (s *Scanner) createContainer(image string) (string, error) {
	args := []string{"create"}
	if s.ContainerLabel != "" {
		args = append(args, "--label", ContainerLabelKey+"="+s.ContainerLabel)
	}
	args = append(args, image, "veris-bundlescan-never-started")
	out, err := s.cli().Output(args...)
	if err != nil {
		return "", fmt.Errorf("bundle scan cannot create a container from %s: %w", image, err)
	}
	ctr := strings.TrimSpace(string(out))
	if ctr == "" {
		return "", fmt.Errorf("bundle scan cannot create a container from %s: no id returned", image)
	}
	return ctr, nil
}

// removeContainer is best-effort: a leaked created container holds disk, not
// a name the next run wants, and the scan's own verdict matters more.
func (s *Scanner) removeContainer(ctr string) {
	_, _ = s.cli().Output("rm", "-f", ctr)
}

// copyOut extracts one file. `docker cp <ctr>:<path> -` writes a single-entry
// tar on stdout; -L follows a symlinked bundle to the file holding the bytes.
func (s *Scanner) copyOut(ctx context.Context, ctr, containerPath string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	rc, err := s.cli().Stream(cctx, "cp", "-L", ctr+":"+containerPath, "-")
	if err != nil {
		return nil, err
	}
	var content []byte
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = rc.Close()
			return nil, fmt.Errorf("docker cp: %w", err)
		}
		if !hdr.FileInfo().Mode().IsRegular() {
			continue
		}
		if hdr.Size > maxBundleSize {
			_ = rc.Close()
			return nil, fmt.Errorf("%d bytes is larger than any CA bundle (limit %d)",
				hdr.Size, maxBundleSize)
		}
		if content, err = io.ReadAll(tr); err != nil {
			_ = rc.Close()
			return nil, fmt.Errorf("docker cp: %w", err)
		}
	}
	if err := rc.Close(); err != nil {
		return nil, err
	}
	if content == nil {
		return nil, errors.New("docker cp returned no file content")
	}
	return content, nil
}

func (s *Scanner) cachePath(id string) string {
	// The ID is "sha256:<hex>"; the colon is not a filename character worth
	// keeping.
	return filepath.Join(s.CacheDir, strings.ReplaceAll(id, ":", "-")+".json")
}

func (s *Scanner) readCache(id string) ([]cachedMatch, bool) {
	if s.CacheDir == "" {
		return nil, false
	}
	raw, err := os.ReadFile(s.cachePath(id))
	if err != nil {
		return nil, false
	}
	var entry cacheEntry
	// A cache that cannot be read is a cache miss, never an error: the export
	// scan rebuilds it from scratch. An entry from an older rule table is a
	// miss too -- its match set predates the rules now in force.
	if json.Unmarshal(raw, &entry) != nil || entry.ImageID != id ||
		entry.Rules != rulesFingerprint() {
		return nil, false
	}
	return entry.Matches, true
}

// writeCache is advisory: a cache that cannot be written costs the next run
// an export, nothing else.
func (s *Scanner) writeCache(id string, matches []tarMatch) {
	if s.CacheDir == "" {
		return
	}
	entry := cacheEntry{ImageID: id, Rules: rulesFingerprint(), Matches: []cachedMatch{}}
	for _, m := range matches {
		entry.Matches = append(entry.Matches, cachedMatch{SDK: m.rule.SDK, Path: "/" + m.path})
	}
	raw, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return
	}
	if os.MkdirAll(s.CacheDir, 0o755) != nil {
		return
	}
	_ = os.WriteFile(s.cachePath(id), raw, 0o644)
}

// tarMatch is one table hit found while streaming the export. content is nil
// when the single pass could not hold the bytes -- a link target outside the
// stashed basenames -- and the caller repairs that with a targeted docker cp.
type tarMatch struct {
	rule    rule
	path    string // cleaned, root-relative
	content []byte
}

// tarLink is a symlink or hardlink member, kept so a matched path can be
// chased to the member that holds the bytes.
type tarLink struct {
	target  string
	symlink bool
}

// maxLinkHops bounds a link chase. Real images use one hop; a chain of eight
// is a loop or an attack, and either way not a bundle to patch.
const maxLinkHops = 8

// maxStashedFiles caps how many bundle-named files the pass holds in memory.
// A normal image carries a handful; past the cap the pass stops stashing and
// lets the docker-cp repair path fetch what a match turns out to need, so the
// cap costs time, never coverage.
const maxStashedFiles = 128

// stashNames are the basenames worth holding as the archive streams past: the
// table's own, plus the system-bundle names a packaged cacert.pem is commonly
// a symlink to. A target outside this set is re-fetched with docker cp
// afterwards, so the list is an optimisation, not a correctness boundary.
var stashNames = map[string]bool{
	"cacert.pem":          true,
	"ca-certificates.crt": true,
	"cacerts.txt":         true,
	"ca-bundle.crt":       true,
	"ca-bundle.pem":       true,
	"cert.pem":            true,
}

// scanExportTar streams the exported filesystem exactly once: match paths
// against the table, hold the bytes of anything a match could resolve to, and
// chase links afterwards. One pass, because the stream cannot be rewound and
// a second export doubles the most expensive step.
func scanExportTar(r io.Reader) ([]tarMatch, error) {
	tr := tar.NewReader(r)
	stash := make(map[string][]byte)
	links := make(map[string]tarLink)
	var matches []tarMatch

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read the export: %w", err)
		}
		name := cleanTarPath(hdr.Name)
		rl, matched := matchRule(name)

		switch hdr.Typeflag {
		case tar.TypeSymlink:
			links[name] = tarLink{target: hdr.Linkname, symlink: true}
		case tar.TypeLink:
			links[name] = tarLink{target: hdr.Linkname}
		default:
			if !hdr.FileInfo().Mode().IsRegular() {
				// A directory or device wearing a bundle's name is not a
				// bundle; nothing to patch and nothing to chase.
				continue
			}
			if matched && hdr.Size > maxBundleSize {
				return nil, fmt.Errorf(
					"%s bundle at /%s: %d bytes is larger than any CA bundle (limit %d)",
					rl.SDK, name, hdr.Size, maxBundleSize)
			}
			if stashNames[path.Base(name)] && hdr.Size <= maxBundleSize &&
				len(stash) < maxStashedFiles {
				content, err := io.ReadAll(tr)
				if err != nil {
					return nil, fmt.Errorf("read /%s from the export: %w", name, err)
				}
				stash[name] = content
			}
		}
		if matched {
			matches = append(matches, tarMatch{rule: rl, path: name})
		}
	}

	for i := range matches {
		final, err := resolveLinks(matches[i].path, links)
		if err != nil {
			return nil, fmt.Errorf("%s bundle at /%s: %w", matches[i].rule.SDK, matches[i].path, err)
		}
		matches[i].content = stash[final]
	}
	return matches, nil
}

// resolveLinks chases a matched path through the archive's link members to
// the one that holds bytes. Targets are resolved WITHIN the archive: a chain
// that climbs out of the root is rejected, because the archive root is the
// container's whole filesystem and outside it there is nothing legitimate to
// point at.
func resolveLinks(p string, links map[string]tarLink) (string, error) {
	cur := p
	for hop := 0; hop <= maxLinkHops; hop++ {
		link, ok := links[cur]
		if !ok {
			return cur, nil
		}
		var next string
		if link.symlink && !strings.HasPrefix(link.target, "/") {
			next = path.Join(path.Dir(cur), link.target)
		} else {
			// Absolute symlinks and hardlink names are archive-root-relative.
			next = cleanTarPath(link.target)
		}
		if next == ".." || strings.HasPrefix(next, "../") {
			return "", fmt.Errorf("link %s points outside the image root (%s)", cur, link.target)
		}
		cur = next
	}
	return "", fmt.Errorf("more than %d link hops from %s", maxLinkHops, p)
}
