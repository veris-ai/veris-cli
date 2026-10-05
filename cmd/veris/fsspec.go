package main

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/veris-ai/veris-cli/internal/api"
	"github.com/veris-ai/veris-cli/internal/cfg"
	"github.com/veris-ai/veris-cli/internal/cli"
	"github.com/veris-ai/veris-cli/internal/discovery"
)

// filesystemService is the name the control plane gives the member that
// holds a sandbox's folders. It is a service like the twins -- it has a
// status, a URL and a control URL -- but its URL is a WebDAV root for
// people and tools, not a vendor base URL for the code under test, so
// every place that hands service URLs to a command skips it by this name
// (discovery.ToConfig, the host tier's config, included).
const filesystemService = discovery.FilesystemService

// Folder source kinds, as the engine spells them.
const (
	folderGit   = "git"
	folderGCS   = "gcs"
	folderPath  = "path" // a local directory, uploaded by up
	folderEmpty = "empty"
)

// fsSpecGrammar is --fs's value, as the usage lines spell it.
const fsSpecGrammar = "[NAME=]SOURCE[@REF][:root]"

// fsFlagHelp is --fs's help on up and env create, the same words in both.
const fsFlagHelp = "folder of the sandbox's filesystem, " + fsSpecGrammar + "; SOURCE is a git URL or a local directory (uploaded first); a gs:// prefix needs auth: and goes in the config; repeatable, and replaces the config's filesystem: list"

// folderSpec is one folder of the sandbox's filesystem as a flag or the
// project file described it, resolved as far as the CLI can without the
// control plane: the kind of source, the name the folder will have, and
// for a local directory its absolute path.
type folderSpec struct {
	Name   string
	Kind   string // folderGit, folderGCS, folderPath or folderEmpty
	Source string // the git URL, gs:// prefix, or local path as given
	Dir    string // folderPath only: the directory, absolute
	Ref    string // folderGit only
	Root   bool
	Auth   string // config only: the environment secret the engine fetches with
}

// parseFolderSpec reads one --fs value, [NAME=]SOURCE[@REF][:root]. A
// local directory is resolved against cwd and must exist (a symlink to one
// is taken as typed; the upload follows it). A gs:// source is refused:
// the control plane wants a secret named with it, which only the config's
// auth: carries. A git source is held to the forms the control plane
// fetches (https, ssh, scp-style) before anything is uploaded. The
// grammar's two delimiters collide with git's own spelling --
// `git@host:org/repo.git` has both an @ and a : -- so the suffix is
// stripped only when it is exactly `:root`, NAME= is read only when the
// text before the first `=` has no `/`, `:` or `@` (a URL's query string
// cannot be a name), and a ref is taken only from a git source, after the
// start of the repository path.
//
// No error says the spec back as typed: a credential in it (refused, the
// URL being recorded in the clear) is masked in every one, prefix and all.
func parseFolderSpec(spec, cwd string) (folderSpec, error) {
	text := spec
	shown := redactCredentials(spec)
	var f folderSpec
	if rest, ok := strings.CutSuffix(text, ":root"); ok {
		f.Root = true
		text = rest
	}
	if i := strings.Index(text, "="); i >= 0 && !strings.ContainsAny(text[:i], "/:@") {
		f.Name = text[:i]
		text = text[i+1:]
		if err := checkFolderName(f.Name); err != nil {
			return f, fmt.Errorf("--fs %s: %w", shown, err)
		}
	}
	switch {
	case text == "":
		if f.Name == "" {
			return f, fmt.Errorf("--fs %s: an empty folder needs a name, NAME=", shown)
		}
		f.Kind = folderEmpty
		return f, nil
	case strings.HasPrefix(text, "gs://"):
		// The control plane refuses a gs:// folder that names no secret to
		// read the bucket with (422), and the flag has no place for one, so
		// the refusal lands here -- before any local folder is uploaded.
		return f, fmt.Errorf("--fs %s: a gs:// source needs auth: naming the secret the engine reads it with, which --fs cannot carry; put it in the config under filesystem: as {gcs: %s, auth: SECRET_NAME}", shown, redactCredentials(text))
	case looksLikeGit(text, cwd):
		f.Kind = folderGit
		f.Source, f.Ref = splitGitRef(text)
		if err := checkGitSource(f.Source); err != nil {
			return f, fmt.Errorf("--fs %s: %w", shown, err)
		}
		if f.Name == "" {
			f.Name = gitFolderName(f.Source)
		}
	default:
		f.Kind, f.Source = folderPath, text
		dir := text
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(cwd, dir)
		}
		info, err := os.Stat(dir)
		switch {
		case err != nil:
			return f, fmt.Errorf("--fs %s: no such directory (SOURCE is a git URL, a gs:// prefix or a local directory)", redactCredentials(text))
		case !info.IsDir():
			return f, fmt.Errorf("--fs %s: not a directory", redactCredentials(text))
		}
		f.Dir = dir
		if f.Name == "" {
			f.Name = filepath.Base(dir)
		}
	}
	if err := checkFolderName(f.Name); err != nil {
		return f, fmt.Errorf("--fs %s: %w", shown, err)
	}
	return f, nil
}

// scpGit is the scp-style git spelling, `user@host:path`, as the control
// plane accepts it.
var scpGit = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9._-]+:`)

// looksLikeGit is whether text names a git source: a URL scheme, the
// scp-style `user@host:path`, or -- when no local directory has that name
// -- a path ending in .git. A scheme git has but the control plane does
// not fetch (http://, git://) is still git here, so checkGitSource can say
// which forms work rather than "no such directory".
func looksLikeGit(text, cwd string) bool {
	for _, p := range []string{"ssh://", "git://", "http://", "https://"} {
		if strings.HasPrefix(text, p) {
			return true
		}
	}
	if scpGit.MatchString(text) {
		return true
	}
	if !strings.HasSuffix(text, ".git") && !strings.Contains(text, ".git@") {
		return false
	}
	dir := text
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(cwd, dir)
	}
	info, err := os.Stat(dir)
	return err != nil || !info.IsDir()
}

// checkGitSource holds a git source to what the control plane fetches:
// https://host/path, ssh://host/path or user@host:path, with no credential
// in it (the server refuses one with 422, since the URL is recorded in the
// clear; auth: in the config names a secret instead). Checked here so a
// refusal comes before any local folder is uploaded. Its errors name the
// source with any credential masked.
func checkGitSource(src string) error {
	shown := redactCredentials(src)
	switch {
	case strings.HasPrefix(src, "https://"):
		u, err := url.Parse(src)
		if err != nil || u.Host == "" || strings.Trim(u.Path, "/") == "" {
			return fmt.Errorf("%s is not a repository URL (https://host/org/repo)", shown)
		}
		if u.User != nil {
			return fmt.Errorf("a credential does not belong in the git URL %s; name an environment secret with auth: in the config's filesystem: entry instead", shown)
		}
	case strings.HasPrefix(src, "ssh://"):
		u, err := url.Parse(src)
		if err != nil || u.Host == "" || strings.Trim(u.Path, "/") == "" {
			return fmt.Errorf("%s is not a repository URL (ssh://git@host/org/repo)", shown)
		}
		if _, hasPassword := u.User.Password(); hasPassword {
			return fmt.Errorf("a credential does not belong in the git URL %s; name an environment secret with auth: in the config's filesystem: entry instead", shown)
		}
	case scpGit.MatchString(src):
		if rest := src[strings.Index(src, ":")+1:]; rest == "" || strings.HasPrefix(rest, "-") {
			return fmt.Errorf("%s names no repository path", shown)
		}
	default:
		return fmt.Errorf("%s is not a git URL the sandbox can fetch: use https://host/org/repo, ssh://git@host/org/repo or git@host:org/repo (http:// and git:// are not fetched)", shown)
	}
	return nil
}

// redactCredentials is text -- a --fs spec, or a source in it -- with the
// credential a URL's userinfo may carry masked as xxxxx, for an error to
// show. With a scheme, the userinfo is the authority's up to its last @: a
// password is masked after the user, and over anything but ssh:// a lone
// user is masked whole (https://TOKEN@host is how a token is spelled). With
// none, a user:password@ ahead of the first / is masked after the user;
// scp-style git@host:path has no password and is shown as typed.
func redactCredentials(text string) string {
	if i := strings.Index(text, "://"); i >= 0 {
		start := i + 3
		end := len(text)
		if j := strings.Index(text[start:], "/"); j >= 0 {
			end = start + j
		}
		at := strings.LastIndex(text[start:end], "@")
		if at < 0 {
			return text
		}
		user, rest := text[start:start+at], text[start+at:]
		scheme := strings.ToLower(text[:i])
		if k := strings.LastIndexAny(scheme, "=:"); k >= 0 {
			scheme = scheme[k+1:] // after a NAME=
		}
		switch name, _, hasPassword := strings.Cut(user, ":"); {
		case hasPassword:
			user = name + ":xxxxx"
		case scheme != "ssh":
			user = "xxxxx"
		}
		return text[:start] + user + rest
	}
	end := len(text)
	if j := strings.Index(text, "/"); j >= 0 {
		end = j
	}
	at := strings.Index(text[:end], "@")
	if at < 0 {
		return text
	}
	if name, _, hasPassword := strings.Cut(text[:at], ":"); hasPassword {
		return name + ":xxxxx" + text[at:]
	}
	return text
}

// splitGitRef takes the @REF off a git source. A URL's own @ sits in its
// authority -- `user@host` before the first `/` after `://`, or before the
// `:` of scp-style `user@host:path` -- so the ref's @ is the last one past
// the start of the repository path. A ref may hold slashes:
// `https://github.com/acme/app.git@feature/foo` yields feature/foo, and
// `git@github.com:acme/app.git` keeps its user with no ref.
func splitGitRef(text string) (source, ref string) {
	pathStart := 0
	if i := strings.Index(text, "://"); i >= 0 {
		rest := text[i+3:]
		slash := strings.Index(rest, "/")
		if slash < 0 {
			return text, ""
		}
		pathStart = i + 3 + slash
	} else if colon := strings.Index(text, ":"); colon >= 0 {
		pathStart = colon
	}
	at := strings.LastIndex(text, "@")
	if at <= pathStart {
		return text, ""
	}
	return text[:at], text[at+1:]
}

// gitFolderName is a repository URL's last segment without .git: the name
// `git clone` would give the directory.
func gitFolderName(source string) string {
	s := strings.TrimRight(source, "/")
	s = strings.TrimSuffix(s, ".git")
	if i := strings.LastIndexAny(s, "/:"); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// folderNamePattern is the control plane's rule for a folder's name (one
// path segment, and the suffix of the variable its credential rides in).
var folderNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// checkFolderName is the rule a folder's name must meet, the control
// plane's own, checked here so a name derived from a local directory
// ("My App") is refused before the directory is uploaded rather than by a
// 422 after it.
func checkFolderName(name string) error {
	switch {
	case name == "" || name == "." || name == "..":
		return fmt.Errorf("folder name %q is not a name", name)
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("folder name %q must be one path segment", name)
	case !folderNamePattern.MatchString(name):
		return fmt.Errorf("folder name %q must start with a letter or digit and hold only letters, digits, '.', '_' and '-' (64 at most); name it with NAME= on --fs, or name: in the config", name)
	}
	return nil
}

// foldersFromFlags is every --fs value parsed, refused together when two
// name the same folder or two claim :root -- a usage error, in main's
// "veris: …" voice, since the fix is on the command line.
func foldersFromFlags(specs []string, cwd string) ([]folderSpec, error) {
	out := make([]folderSpec, 0, len(specs))
	for _, spec := range specs {
		f, err := parseFolderSpec(spec, cwd)
		if err != nil {
			return nil, &cli.UsageError{Msg: err.Error()}
		}
		out = append(out, f)
	}
	if err := checkFolders(out); err != nil {
		return nil, &cli.UsageError{Msg: "--fs: " + err.Error()}
	}
	return out, nil
}

// foldersFromConfig is the environment config's filesystem: list, with
// each path: resolved against the project directory. A folder that names
// two sources, or none and no name, is a config error naming the entry.
func foldersFromConfig(dir string, conf []cfg.FolderConfig) ([]folderSpec, error) {
	out := make([]folderSpec, 0, len(conf))
	for i, c := range conf {
		f := folderSpec{Name: c.Name, Ref: c.Ref, Root: c.Root, Auth: c.Auth}
		sources := 0
		for _, set := range []bool{c.Git != "", c.GCS != "", c.Path != ""} {
			if set {
				sources++
			}
		}
		if sources > 1 {
			return nil, fmt.Errorf("filesystem[%d] names more than one of git, gcs and path", i)
		}
		switch {
		case c.Git != "":
			f.Kind, f.Source = folderGit, c.Git
			if err := checkGitSource(c.Git); err != nil {
				return nil, fmt.Errorf("filesystem[%d]: %w", i, err)
			}
			if f.Name == "" {
				f.Name = gitFolderName(c.Git)
			}
		case c.GCS != "":
			f.Kind, f.Source = folderGCS, c.GCS
			// The control plane refuses a gs:// folder with no secret to
			// read it with (422); said here, before any upload is sent.
			if f.Auth == "" {
				return nil, fmt.Errorf("filesystem[%d]: gcs %s needs auth: naming the secret the engine reads it with", i, redactCredentials(c.GCS))
			}
			if f.Name == "" {
				f.Name = path.Base(strings.TrimRight(strings.TrimPrefix(c.GCS, "gs://"), "/"))
			}
		case c.Path != "":
			f.Kind, f.Source, f.Dir = folderPath, c.Path, projectPath(dir, c.Path)
			info, err := os.Stat(f.Dir)
			if err != nil || !info.IsDir() {
				return nil, fmt.Errorf("filesystem[%d]: path %s is not a directory", i, c.Path)
			}
			if f.Name == "" {
				f.Name = filepath.Base(f.Dir)
			}
		default:
			if f.Name == "" {
				return nil, fmt.Errorf("filesystem[%d] has no git, gcs or path and no name", i)
			}
			f.Kind = folderEmpty
		}
		if f.Ref != "" && f.Kind != folderGit {
			return nil, fmt.Errorf("filesystem[%d]: ref applies to a git source", i)
		}
		if err := checkFolderName(f.Name); err != nil {
			return nil, fmt.Errorf("filesystem[%d]: %w", i, err)
		}
		out = append(out, f)
	}
	if err := checkFolders(out); err != nil {
		return nil, fmt.Errorf("filesystem: %w", err)
	}
	return out, nil
}

// checkFolders holds a list to the server's rules the CLI can check first:
// unique names, at most one root.
func checkFolders(folders []folderSpec) error {
	seen := map[string]bool{}
	root := ""
	for _, f := range folders {
		if seen[f.Name] {
			return fmt.Errorf("two folders are named %q; give one a NAME=", f.Name)
		}
		seen[f.Name] = true
		if f.Root {
			if root != "" {
				return fmt.Errorf("both %s and %s are :root; only one folder can be", root, f.Name)
			}
			root = f.Name
		}
	}
	return nil
}

// folderRequest is the create request's filesystem list: each spec as the
// control plane reads it, a local directory replaced by the id its upload
// was given. The name is always sent, so what the server records is what
// up printed.
func folderRequest(folders []folderSpec, uploads map[string]string) []api.FilesystemFolder {
	out := make([]api.FilesystemFolder, 0, len(folders))
	for _, f := range folders {
		item := api.FilesystemFolder{Name: f.Name, Ref: f.Ref, Root: f.Root, Auth: f.Auth}
		switch f.Kind {
		case folderGit:
			item.Git = f.Source
		case folderGCS:
			item.GCS = f.Source
		case folderPath:
			item.Upload = uploads[f.Name]
		}
		out = append(out, item)
	}
	return out
}

// folderLabel is a folder on one line of up's output: "app (git …@main,
// root)", "fixtures (path fixtures/data)".
func folderLabel(f folderSpec) string {
	what := f.Kind
	if f.Source != "" {
		what += " " + f.Source
	}
	if f.Ref != "" {
		what += "@" + f.Ref
	}
	if f.Root {
		what += ", root"
	}
	return f.Name + " (" + what + ")"
}

// tarExcluded is what a local folder's upload leaves out: this tool's own
// per-folder file, which holds sandbox ids (capabilities), and its locks.
// Everything else goes, .git included -- the engine builds its own
// baseline beside the folder and a repository is more useful with its
// history. Matched at any depth, since a folder may hold a project of its
// own.
func tarExcluded(rel string) bool {
	base := path.Base(rel)
	return path.Base(path.Dir(rel)) == ".veris" && (base == "twin.local.yaml" || strings.HasSuffix(base, ".lock"))
}

// tarSummary is what tarDir packed.
type tarSummary struct {
	Files   int
	Bytes   int64
	Skipped []string // entries that are neither files, directories nor symlinks
	// Shadowed is the top-level names an unpack left out because another
	// folder lands there (attach's root folder).
	Shadowed []string
}

// tarDir writes dir as a gzip tar to w: directories, regular files and
// symlinks (as links, never followed), paths relative to dir with forward
// slashes. Sockets and devices are skipped and named in the summary. dir
// itself is resolved first: WalkDir reads its root with Lstat, so a source
// named through a symlink (`current -> releases/v3`) would otherwise be
// one link entry and no files, and the sandbox would get an empty folder.
func tarDir(dir string, w io.Writer) (tarSummary, error) {
	var sum tarSummary
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return sum, err
	}
	// The archive may be written inside the tree it packs (TMPDIR under the
	// source, or the temporary directory itself uploaded); it is never
	// packed into itself.
	var self fs.FileInfo
	if f, ok := w.(*os.File); ok {
		self, _ = f.Stat()
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if tarExcluded(rel) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if self != nil && os.SameFile(info, self) {
			return nil
		}
		link := ""
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		case !info.IsDir() && !info.Mode().IsRegular():
			sum.Skipped = append(sum.Skipped, rel)
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = rel
		if info.IsDir() {
			hdr.Name += "/"
		}
		// Ownership is this machine's and means nothing to the engine, which
		// owns everything it unpacks; names of users here stay here.
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		hdr.Format = tar.FormatPAX
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		n, err := io.Copy(tw, f)
		f.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		sum.Files++
		sum.Bytes += n
		return nil
	})
	if err != nil {
		return sum, err
	}
	if err := tw.Close(); err != nil {
		return sum, err
	}
	return sum, gz.Close()
}

// byteSize renders n for a line of output: "12.3 MB", "640 kB", "72 B".
func byteSize(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}
