package bundlescan

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Mount is one parsed -v flag, in the shapes `docker run` accepts: dst alone
// (an anonymous volume), src:dst and src:dst:opts, where src is a host path
// or a named volume.
type Mount struct {
	Raw    string
	Source string // host path or volume name; "" for an anonymous volume
	Dest   string
	// HostDir marks a source that is an existing host directory, and HostFile
	// one that is an existing regular file -- the two kinds of mount the scan
	// can look inside.
	HostDir  bool
	HostFile bool
	// resolvedSource is the symlink-resolved source the scan reads. Docker
	// resolves a symlinked source the same way before binding it, and WalkDir
	// does not follow a symlink root -- walking the raw source would scan
	// nothing while claiming the mount as covered.
	resolvedSource string
}

// ParseVolume splits a -v value. It validates nothing docker would refuse
// anyway; it only needs the destination, and whether the source is a host
// directory it can walk.
func ParseVolume(spec string) Mount {
	parts := strings.SplitN(spec, ":", 3)
	if len(parts) == 1 {
		return Mount{Raw: spec, Dest: cleanDest(parts[0])}
	}
	m := Mount{Raw: spec, Source: parts[0], Dest: cleanDest(parts[1])}
	// Docker reads a source without a path separator as a named volume; only
	// a path can be stat'ed and walked.
	if strings.Contains(m.Source, "/") {
		if resolved, err := filepath.EvalSymlinks(m.Source); err == nil {
			if info, err := os.Stat(resolved); err == nil {
				switch {
				case info.IsDir():
					m.HostDir = true
					m.resolvedSource = resolved
				case info.Mode().IsRegular():
					m.HostFile = true
					m.resolvedSource = resolved
				}
			}
		}
	}
	return m
}

func cleanDest(dst string) string {
	if dst == "" {
		return ""
	}
	return path.Clean(dst)
}

// maxWalkDepth bounds how deep below a bind source the walk descends. Every
// known bundle sits within a dozen components of its site-packages or gem
// root; deeper is a haystack the walk deliberately stays out of.
const maxWalkDepth = 12

// maxWalkFiles bounds the walk in breadth, and exceeding it is an error
// rather than a truncation: a walk that quietly stopped would report "no
// bundles" for a tree it never finished reading. A variable only so tests can
// trip it without building a fifty-thousand-file tree.
var maxWalkFiles = 50000

// ScanVolume reads a bind mount with the same table the image scan uses. The
// mount shadows whatever the image holds under the same destination, so the
// copy found here is the one the SDK will actually read. A directory source
// is walked; a regular-file source is judged by its destination alone.
func ScanVolume(m Mount) ([]Candidate, error) {
	if m.HostFile {
		return scanFileMount(m)
	}
	if !m.HostDir {
		return nil, nil
	}
	root := m.resolvedSource
	if root == "" {
		root = m.Source
	}
	w := &volumeWalk{m: m, root: root, chain: map[string]bool{root: true}}
	if err := w.walk(root, ""); err != nil {
		return nil, err
	}
	return w.out, nil
}

// scanFileMount handles a host FILE bound directly at a known bundle path:
// the destination alone anchors the match, and the file's bytes are what the
// SDK will read there. A lookalike that fails validation refuses rather than
// skips -- silently ignoring the mount would leave the effective bundle
// unpatched with everything looking healthy.
func scanFileMount(m Mount) ([]Candidate, error) {
	rl, ok := matchRule(strings.TrimPrefix(m.Dest, "/"))
	if !ok {
		return nil, nil
	}
	src := m.resolvedSource
	if src == "" {
		src = m.Source
	}
	content, err := readBounded(src)
	if err != nil {
		return nil, fmt.Errorf("%s bundle at %s (-v %s): %w", rl.SDK, m.Dest, m.Raw, err)
	}
	if err := validate(content); err != nil {
		return nil, fmt.Errorf("%s bundle at %s (-v %s): %v", rl.SDK, m.Dest, m.Raw, err)
	}
	return []Candidate{{
		SDK:           rl.SDK,
		ContainerPath: m.Dest,
		Content:       content,
		Origin:        src,
		mountDest:     m.Dest,
	}}, nil
}

// volumeWalk carries one mount's walk state. The file budget spans every
// directory the walk enters, symlinked or not, and is the global bound on the
// walk's work; chain holds only the ACTIVE recursion's resolved directories,
// because two sibling links to one target are two distinct container-visible
// paths and each must be walked -- only a link back into its own ancestry is
// a cycle.
type volumeWalk struct {
	m     Mount
	root  string
	files int
	chain map[string]bool // resolved directories the current recursion is inside
	out   []Candidate
}

// walk scans one real directory. prefix is the mount-relative path this
// directory answers to -- "" for the root, the symlink's own path for a tree
// reached through one -- because the container resolves the link too, and the
// symlink-side path is where the bundle is read and over-mounted.
func (w *volumeWalk) walk(dir, prefix string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("bundle scan of %s: %w", w.m.Source, err)
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if prefix != "" {
			if rel == "." {
				rel = prefix
			} else {
				rel = prefix + "/" + rel
			}
		}
		if d.IsDir() {
			if rel != "." && strings.Count(rel, "/")+1 >= maxWalkDepth {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			if followed, err := w.follow(p, rel); followed || err != nil {
				return err
			}
			// Not a directory to descend into: fall through to the file
			// handling, where stat and read chase the link to its bytes.
		}
		w.files++
		if w.files > maxWalkFiles {
			return fmt.Errorf(
				"bundle scan of %s exceeded its %d-file budget before finishing; "+
					"narrow the -v mount, or drop --patch-bundled-cas",
				w.m.Source, maxWalkFiles)
		}
		// The table is matched against the path the CONTAINER sees: the mount
		// destination can supply the anchoring SDK directory itself, so the
		// source-relative path alone under-matches.
		cpath := path.Join(w.m.Dest, rel)
		rl, ok := matchRule(strings.TrimPrefix(cpath, "/"))
		if !ok {
			return nil
		}
		content, err := readBounded(p)
		if err != nil {
			return fmt.Errorf("%s bundle at %s: %w", rl.SDK, p, err)
		}
		if err := validate(content); err != nil {
			return fmt.Errorf("%s bundle at %s: %v", rl.SDK, p, err)
		}
		w.out = append(w.out, Candidate{
			SDK:           rl.SDK,
			ContainerPath: cpath,
			Content:       content,
			Origin:        p,
			mountDest:     w.m.Dest,
		})
		return nil
	})
}

// follow descends into a MOUNT-RELATIVE directory symlink whose target stays
// INSIDE the mount: the container resolves such a link the same way, so a
// bundle behind it is really reachable at the link's own path -- which
// WalkDir alone would never visit. An absolute link is never followed, even
// when its host target sits inside the root: the container resolves it
// against the CONTAINER's root, not the bind source, so the host-side target
// says nothing about what the container finds there. A relative link
// escaping the root is left alone for the same reason, and one back into its
// own recursion chain is a cycle.
func (w *volumeWalk) follow(p, rel string) (bool, error) {
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		// Dangling or looping on itself; nothing behind it to scan.
		return false, nil
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return false, nil
	}
	target, err := os.Readlink(p)
	if err != nil || filepath.IsAbs(target) {
		return true, nil
	}
	if resolved != w.root && !strings.HasPrefix(resolved, w.root+string(filepath.Separator)) {
		return true, nil
	}
	if w.chain[resolved] {
		return true, nil
	}
	w.chain[resolved] = true
	werr := w.walk(resolved, rel)
	delete(w.chain, resolved)
	return true, werr
}

// readBounded reads a candidate file, refusing the oversized before reading:
// the size bound is what keeps a lookalike from costing memory.
func readBounded(p string) ([]byte, error) {
	info, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxBundleSize {
		return nil, fmt.Errorf("%d bytes is larger than any CA bundle (limit %d)",
			info.Size(), maxBundleSize)
	}
	return os.ReadFile(p)
}

// Collect is the whole discovery for one run: the image scan, a walk of each
// -v bind, and the shadowing rules that decide which copy of a bundle the SDK
// will actually read.
func (s *Scanner) Collect(ctx context.Context, image string, volumes []string) ([]Candidate, error) {
	mounts := make([]Mount, 0, len(volumes))
	for _, v := range volumes {
		mounts = append(mounts, ParseVolume(v))
	}

	imageCands, err := s.ScanImage(ctx, image)
	if err != nil {
		return nil, err
	}

	all := imageCands
	for _, m := range mounts {
		volCands, err := ScanVolume(m)
		if err != nil {
			return nil, err
		}
		all = append(all, volCands...)
	}
	all, err = dropShadowed(all, mounts)
	if err != nil {
		return nil, err
	}
	all = dedupeByPath(all)
	if err := refuseExactMounts(all, mounts); err != nil {
		return nil, err
	}
	return all, nil
}

// dropShadowed keeps only the candidates whose bytes the container will
// actually see. A candidate is governed by the DEEPEST mount covering its
// container path -- docker lays binds parent-first, so that mount masks the
// image and every shallower bind alike:
//
//   - the candidate's own mount (or none, for an untouched image path): the
//     candidate is the effective copy;
//   - a host-directory mount: its own walk found the effective copy, or the
//     file does not exist in the container at all;
//   - an anonymous volume: docker copies it up from the IMAGE, so the image
//     candidate stays effective and a shallower bind's copy is masked;
//   - anything the scan cannot see inside (a named volume): a known bundle
//     would ship unpatched with everything looking healthy, so that refuses
//     instead.
//
// A file bind AT a candidate's exact path is the deepest cover there can be:
// its own candidate speaks for the path and every other copy is masked. Any
// other exact-destination mount is the conflict refuseExactMounts judges
// with its own message.
func dropShadowed(cands []Candidate, mounts []Mount) ([]Candidate, error) {
	var out []Candidate
	for _, c := range cands {
		if c.mountDest == c.ContainerPath {
			out = append(out, c)
			continue
		}
		if ex := mountAt(c.ContainerPath, mounts); ex != nil {
			if !ex.HostFile {
				out = append(out, c)
			}
			continue
		}
		gov := deepestCovering(c.ContainerPath, mounts)
		switch {
		case gov == nil || gov.Dest == c.mountDest:
			out = append(out, c)
		case gov.HostDir:
			// Dropped: the governing mount's own scan speaks for this path.
		case gov.Source == "":
			if c.mountDest == "" {
				out = append(out, c)
			}
		default:
			return nil, fmt.Errorf(
				"--patch-bundled-cas found the %s bundle at %s, but -v %s mounts "+
					"over it and the scan cannot see inside that mount; bind a host "+
					"directory instead, or drop the flag",
				c.SDK, c.ContainerPath, gov.Raw)
		}
	}
	return out, nil
}

// mountAt returns the mount bound exactly at p, or nil.
func mountAt(p string, mounts []Mount) *Mount {
	for i := range mounts {
		if mounts[i].Dest == p {
			return &mounts[i]
		}
	}
	return nil
}

// deepestCovering returns the deepest mount whose destination sits strictly
// above p, or nil. A mount AT p is not a cover: two mounts at one destination
// is the conflict refuseExactMounts judges with its own message.
func deepestCovering(p string, mounts []Mount) *Mount {
	var gov *Mount
	for i := range mounts {
		m := &mounts[i]
		if m.Dest == "" || !strings.HasPrefix(p, m.Dest+"/") {
			continue
		}
		if gov == nil || len(m.Dest) > len(gov.Dest) {
			gov = m
		}
	}
	return gov
}

// dedupeByPath keeps one candidate per container path. Docker applies binds
// parent-first, so when two -v mounts both supply a path, the copy from the
// deeper destination is the one the container sees.
func dedupeByPath(cands []Candidate) []Candidate {
	index := make(map[string]int, len(cands))
	var out []Candidate
	for _, c := range cands {
		i, seen := index[c.ContainerPath]
		if !seen {
			index[c.ContainerPath] = len(out)
			out = append(out, c)
			continue
		}
		if len(c.mountDest) > len(out[i].mountDest) {
			out[i] = c
		}
	}
	return out
}

// refuseExactMounts fails when the user binds EXACTLY a candidate's path
// with something the scan could not read: two mounts at one destination is a
// docker error, and silently dropping ours would leave the user's unpatched
// copy in play. A regular-file bind never lands here -- scanFileMount made
// it the candidate itself, and the run replaces that bind with the patched
// copy.
func refuseExactMounts(cands []Candidate, mounts []Mount) error {
	for _, c := range cands {
		for _, m := range mounts {
			if m.Dest != c.ContainerPath || m.Dest == c.mountDest {
				continue
			}
			return fmt.Errorf(
				"--patch-bundled-cas wants to over-mount the %s bundle at %s, but "+
					"-v %s already mounts that exact path; remove that mount, or "+
					"append the published Veris CA to the file it binds and drop "+
					"the flag", c.SDK, c.ContainerPath, m.Raw)
		}
	}
	return nil
}
