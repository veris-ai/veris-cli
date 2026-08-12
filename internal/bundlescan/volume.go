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
	// HostDir marks a source that is an existing host directory -- the only
	// kind of mount the scan can look inside.
	HostDir bool
	// walkRoot is the symlink-resolved source a HostDir walk reads. Docker
	// resolves a symlinked source the same way before binding it, and WalkDir
	// does not follow a symlink root -- walking the raw source would scan
	// nothing while claiming the mount as covered.
	walkRoot string
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
			if info, err := os.Stat(resolved); err == nil && info.IsDir() {
				m.HostDir = true
				m.walkRoot = resolved
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

// ScanVolume walks a bind mount's host directory with the same table the
// image scan uses. The volume shadows whatever the image holds under the same
// destination, so the copy found here is the one the SDK will actually read.
func ScanVolume(m Mount) ([]Candidate, error) {
	if !m.HostDir {
		return nil, nil
	}
	root := m.walkRoot
	if root == "" {
		root = m.Source
	}
	var out []Candidate
	files := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("bundle scan of %s: %w", m.Source, err)
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && strings.Count(rel, "/")+1 >= maxWalkDepth {
				return fs.SkipDir
			}
			return nil
		}
		files++
		if files > maxWalkFiles {
			return fmt.Errorf(
				"bundle scan of %s exceeded its %d-file budget before finishing; "+
					"narrow the -v mount, or drop --patch-bundled-cas",
				m.Source, maxWalkFiles)
		}
		// The table is matched against the path the CONTAINER sees: the mount
		// destination can supply the anchoring SDK directory itself, so the
		// source-relative path alone under-matches.
		cpath := path.Join(m.Dest, rel)
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
		out = append(out, Candidate{
			SDK:           rl.SDK,
			ContainerPath: cpath,
			Content:       content,
			Origin:        p,
			mountDest:     m.Dest,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
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
//   - anything the scan cannot see inside (a named volume, a host file): a
//     known bundle would ship unpatched with everything looking healthy, so
//     that refuses instead.
func dropShadowed(cands []Candidate, mounts []Mount) ([]Candidate, error) {
	var out []Candidate
	for _, c := range cands {
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

// refuseExactMounts fails when the user already binds EXACTLY a path the run
// would over-mount: two mounts at one destination is a docker error, and
// silently dropping ours would leave the user's unpatched copy in play.
func refuseExactMounts(cands []Candidate, mounts []Mount) error {
	for _, c := range cands {
		for _, m := range mounts {
			if m.Dest == c.ContainerPath {
				return fmt.Errorf(
					"--patch-bundled-cas wants to over-mount the %s bundle at %s, but "+
						"-v %s already mounts that exact path; remove that mount, or "+
						"append the published Veris CA to the file it binds and drop "+
						"the flag", c.SDK, c.ContainerPath, m.Raw)
			}
		}
	}
	return nil
}
