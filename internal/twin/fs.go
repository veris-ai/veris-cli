package twin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// The filesystem member is the one twin that holds folders rather than
// rows. Its control listener serves /veris/health like every other member
// and, under /veris/fs/*, the routes below. An engine or control plane too
// old to have them answers FastAPI's own 404, and every 404 on these routes
// is ErrNotSupported: they are optional by design, and a caller reads that
// one error as "this sandbox has no filesystem routes".

// FSToken is POST /veris/fs/token's answer: Basic auth credentials for the
// filesystem's WebDAV data URL, good until ExpiresAt. Expiry reads it as a
// time when it parses; the string is kept as sent for --json.
type FSToken struct {
	User      string `json:"user"`
	Password  string `json:"password"`
	ExpiresAt string `json:"expires_at"`
}

// Expiry is ExpiresAt as a time, and false when the engine sent something
// that is not RFC 3339.
func (t *FSToken) Expiry() (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05"} {
		if at, err := time.Parse(layout, t.ExpiresAt); err == nil {
			return at, true
		}
	}
	return time.Time{}, false
}

// FSLayout is GET /veris/fs/layout: every folder with its source and
// state. Primary is the folder the agent's working directory is -- the root
// folder when there is one, else the first; Root is the root folder's name,
// nil when none is marked.
type FSLayout struct {
	Primary string     `json:"primary"`
	Root    *string    `json:"root"`
	Folders []FSFolder `json:"folders"`
}

// FSFolder is one folder of the layout. Ready is whether its pull finished;
// Modified whether anything differs from the baseline the engine recorded.
type FSFolder struct {
	Name     string   `json:"name"`
	Root     bool     `json:"root"`
	Source   FSSource `json:"source"`
	Bytes    int64    `json:"bytes"`
	Files    int      `json:"files"`
	Ready    bool     `json:"ready"`
	Modified bool     `json:"modified"`
}

// FSSource is where a folder came from: Kind is git, gcs, upload or empty,
// and URL, Prefix or ID carries the coordinate for the kind; Ref and SHA
// are a git source's branch and the commit the baseline is.
type FSSource struct {
	Kind   string `json:"kind"`
	URL    string `json:"url,omitempty"`
	Prefix string `json:"prefix,omitempty"`
	ID     string `json:"id,omitempty"`
	Ref    string `json:"ref,omitempty"`
	SHA    string `json:"sha,omitempty"`
}

// Where is the source as one line: the URL, prefix or upload id, with the
// ref when there is one.
func (s FSSource) Where() string {
	where := s.URL
	if where == "" {
		where = s.Prefix
	}
	if where == "" {
		where = s.ID
	}
	if where == "" {
		where = s.Kind
	}
	if s.Ref != "" {
		where += "@" + s.Ref
	}
	return where
}

// Diff formats GET /veris/fs/diff takes.
const (
	FSDiffStatus = "status"
	FSDiffPatch  = "patch"
)

// FSToken mints Basic auth credentials for the WebDAV data URL (POST
// /veris/fs/token {ttl_s}). ttl is rounded down to whole seconds and sent
// only when positive; the engine's default is an hour and its ceiling a
// day, and it refuses beyond that itself.
func (c *Client) FSToken(ctx context.Context, ttl time.Duration) (*FSToken, error) {
	body := map[string]any{}
	if ttl > 0 {
		body["ttl_s"] = int(ttl / time.Second)
	}
	var out FSToken
	if err := optional404(c.do(ctx, http.MethodPost, "/veris/fs/token", nil, body, &out)); err != nil {
		return nil, err
	}
	if out.User == "" || out.Password == "" {
		return nil, errors.New("POST /veris/fs/token answered without a user and password")
	}
	return &out, nil
}

// FSLayout is GET /veris/fs/layout.
func (c *Client) FSLayout(ctx context.Context) (*FSLayout, error) {
	var out FSLayout
	if err := optional404(c.do(ctx, http.MethodGet, "/veris/fs/layout", nil, nil, &out)); err != nil {
		return nil, err
	}
	return &out, nil
}

// FSDiff is GET /veris/fs/diff?folder=&format=: the folder's changes
// against its baseline, as the engine sent them -- JSON for FSDiffStatus, a
// unified patch for FSDiffPatch. truncated reports the engine's
// X-Veris-Truncated header, set when a patch was cut at its cap.
func (c *Client) FSDiff(ctx context.Context, folder, format string) (body []byte, truncated bool, err error) {
	if folder == "" {
		return nil, false, errors.New("folder name is required")
	}
	switch format {
	case FSDiffStatus, FSDiffPatch:
	default:
		return nil, false, fmt.Errorf("format %q is not %s or %s", format, FSDiffStatus, FSDiffPatch)
	}
	q := url.Values{"folder": {folder}, "format": {format}}
	resp, err := c.raw(ctx, "/veris/fs/diff", q)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, false, fmt.Errorf("read GET /veris/fs/diff: %w", err)
	}
	truncated, _ = strconv.ParseBool(resp.Header.Get("X-Veris-Truncated"))
	return body, truncated, nil
}

// FSExport is GET /veris/fs/export?folder=: the folder as a gzip tar
// stream, its .git included when the folder has one. The caller closes the
// reader; a non-2xx answer is read and returned as the methods' errors.
func (c *Client) FSExport(ctx context.Context, folder string) (io.ReadCloser, error) {
	if folder == "" {
		return nil, errors.New("folder name is required")
	}
	resp, err := c.raw(ctx, "/veris/fs/export", url.Values{"folder": {folder}})
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// raw is one GET whose 2xx body the caller streams or keeps as bytes: the
// request is built here so the key rides the transport (c.keyed), never the
// request, and a non-2xx answer is read, closed and classified as do does.
// Both routes are optional, so a 404 is ErrNotSupported.
func (c *Client) raw(ctx context.Context, path string, q url.Values) (*http.Response, error) {
	endpoint := c.ControlURL + path + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	hc := c.HTTP
	if hc == nil {
		hc = defaultHTTP
	}
	// A stream has no timeout of its own: a gigabyte export under the 30 s
	// JSON budget would be cut mid-tar. ctx bounds it instead.
	streaming := *c.keyed(hc)
	streaming.Timeout = 0
	resp, err := streaming.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the twin at %s: %w", c.ControlURL, err)
	}
	if resp.StatusCode/100 == 2 {
		return resp, nil
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("read GET %s: %w", path, err)
	}
	return nil, optional404(c.classify(parseError(resp.StatusCode, http.MethodGet, path+"?"+q.Encode(), raw)))
}
