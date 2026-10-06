package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// uploadTimeout bounds one folder upload. The client's own 30 s is for JSON
// round trips; a folder is gigabytes at worst, and the server's limit (2 GiB
// by default) is what bounds the size, so the time is generous rather than
// tight. ctx still cancels it early.
const uploadTimeout = time.Hour

// UploadFolder stores a gzip tar stream as an environment upload (POST
// /v1/environments/{id}/uploads, 201) and returns the id a
// FilesystemFolder's Upload names. name, when non-empty, is sent as ?name=
// so the server can default the folder's name from it.
//
// The body streams rather than going through do, which buffers a JSON
// []byte: size is sent as Content-Length so the server can refuse an
// oversized upload with 413 before reading it. The request is sent exactly
// once, as every non-GET here is, and never follows a redirect -- a
// capability POST that followed one could stream a folder to another host.
// A 404 {"detail":"Not Found"} is a control plane without the route and
// surfaces as *Error so the caller can say so.
func (c *Client) UploadFolder(ctx context.Context, envID, name string, body io.Reader, size int64) (*UploadResponse, error) {
	path := "/v1/environments/" + pathEscape(envID) + "/uploads"
	endpoint := c.Base + path
	if name != "" {
		endpoint += "?" + url.Values{"name": {name}}.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return nil, err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/gzip")
	req.Header.Set("Accept", "application/json")
	if c.Key != "" {
		req.Header.Set("X-API-Key", c.Key)
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	base := c.HTTP
	if base == nil {
		base = http.DefaultClient
	}
	hc := *base
	hc.Timeout = uploadTimeout
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the control plane at %s: %w", c.Base, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("read %s %s: %w", http.MethodPost, endpoint, err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, parseError(http.MethodPost, endpoint, resp, respBody)
	}
	var out UploadResponse
	if err := decode(http.MethodPost, path, respBody, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
