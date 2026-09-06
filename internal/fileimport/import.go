// Package fileimport stages bounded ZIP batches and records acknowledged imports.
package fileimport

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
type Options struct {
	Source     string `json:"source"`
	ControlURL string `json:"control_url"`
	Owner      string `json:"owner"`
	Prefix     string `json:"prefix"`
	BatchBytes int64  `json:"batch_bytes"`
	Checkpoint string `json:"-"`
	Resume     bool   `json:"-"`
}
type Receipt struct {
	Version   int               `json:"version"`
	Options   Options           `json:"options"`
	Files     []File            `json:"files"`
	Completed int               `json:"completed_files"`
	Bytes     int64             `json:"completed_bytes"`
	Pending   []File            `json:"pending,omitempty"`
	Responses []json.RawMessage `json:"batch_responses"`
}

func manifest(root string, checkpoint string) ([]File, error) {
	var files []File
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == checkpoint || path == checkpoint+".lock" {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink is not imported: %s", path)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("not a regular file: %s", path)
		}
		if info.Size() > 1<<30 {
			return fmt.Errorf("%s exceeds the API's 1 GiB file limit", path)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		h := sha256.New()
		n, err := io.Copy(h, f)
		f.Close()
		if err != nil {
			return err
		}
		if n != info.Size() {
			return fmt.Errorf("file changed while reading: %s", path)
		}
		files = append(files, File{filepath.ToSlash(rel), n, hex.EncodeToString(h.Sum(nil))})
		return nil
	})
	return files, err
}

func save(path string, receipt *Receipt) error {
	b, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".import-checkpoint-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func stage(root string, files []File, raw bool) (string, error) {
	f, err := os.CreateTemp("", "veris-file-import-*")
	if err != nil {
		return "", err
	}
	success := false
	defer func() {
		f.Close()
		if !success {
			os.Remove(f.Name())
		}
	}()
	var z *zip.Writer
	if !raw {
		z = zip.NewWriter(f)
	}
	for _, item := range files {
		var out io.Writer = f
		if z != nil {
			h := &zip.FileHeader{Name: item.Path, Method: zip.Store}
			out, err = z.CreateHeader(h)
			if err != nil {
				return "", err
			}
		}
		source, err := os.Open(filepath.Join(root, filepath.FromSlash(item.Path)))
		if err != nil {
			return "", err
		}
		h := sha256.New()
		n, copyErr := io.Copy(io.MultiWriter(out, h), source)
		source.Close()
		if copyErr != nil {
			return "", copyErr
		}
		if n != item.Size || hex.EncodeToString(h.Sum(nil)) != item.SHA256 {
			return "", fmt.Errorf("source changed before upload: %s", item.Path)
		}
	}
	if z != nil {
		if err = z.Close(); err != nil {
			return "", err
		}
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	success = true
	return f.Name(), nil
}

// Run never replays an uncertain POST. Resume skips only batches whose
// successful response was durably recorded for this exact source and target.
func Run(ctx context.Context, httpClient *http.Client, o Options, progress func(*Receipt)) (*Receipt, error) {
	var err error
	o.Source, err = filepath.Abs(o.Source)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(o.Source)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("source must be a directory")
	}
	if o.BatchBytes <= 0 || o.BatchBytes > 1<<30 {
		return nil, fmt.Errorf("batch size must be between 1 byte and 1 GiB")
	}
	if o.Owner == "" || o.Prefix == "" {
		return nil, fmt.Errorf("owner and destination prefix are required")
	}
	u, err := url.Parse(strings.TrimRight(o.ControlURL, "/") + "/veris/files")
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("invalid control URL")
	}
	if err = os.MkdirAll(filepath.Dir(o.Checkpoint), 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(o.Checkpoint+".lock", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("checkpoint is locked; ensure no importer is running before removing %s.lock: %w", o.Checkpoint, err)
	}
	fmt.Fprintln(lock, os.Getpid())
	lock.Close()
	defer os.Remove(o.Checkpoint + ".lock")
	checkpoint, err := filepath.Abs(o.Checkpoint)
	if err != nil {
		return nil, err
	}
	files, err := manifest(o.Source, checkpoint)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("source directory contains no files")
	}
	receipt := &Receipt{Version: 1, Options: o, Files: files}
	previous, err := os.ReadFile(o.Checkpoint)
	if err == nil {
		if !o.Resume {
			return nil, fmt.Errorf("checkpoint exists; use --resume to skip acknowledged files")
		}
		receipt = &Receipt{}
		if err = json.Unmarshal(previous, receipt); err != nil {
			return nil, err
		}
		if receipt.Version != 1 || !reflect.DeepEqual(receipt.Files, files) || receipt.Options.Source != o.Source || receipt.Options.ControlURL != o.ControlURL || receipt.Options.Owner != o.Owner || receipt.Options.Prefix != o.Prefix || receipt.Options.BatchBytes != o.BatchBytes {
			return nil, fmt.Errorf("checkpoint does not match the source, target, or batch settings")
		}
		if len(receipt.Pending) > 0 {
			return receipt, fmt.Errorf("last batch outcome is unknown; reconcile the listed pending files with the service before changing the checkpoint; replay could create revisions")
		}
		if receipt.Completed < 0 || receipt.Completed > len(files) {
			return nil, fmt.Errorf("invalid checkpoint file count")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	} else if o.Resume {
		return nil, fmt.Errorf("no checkpoint to resume")
	}
	if err = save(o.Checkpoint, receipt); err != nil {
		return nil, err
	}
	for receipt.Completed < len(files) {
		if err = ctx.Err(); err != nil {
			return receipt, err
		}
		start, end := receipt.Completed, receipt.Completed
		var n int64
		for end < len(files) && (end == start || n+files[end].Size <= o.BatchBytes) {
			n += files[end].Size
			end++
		}
		batch := files[start:end]
		raw := len(batch) == 1 && n > o.BatchBytes
		staged, err := stage(o.Source, batch, raw)
		if err != nil {
			return receipt, err
		}
		receipt.Pending = batch
		if err = save(o.Checkpoint, receipt); err != nil {
			os.Remove(staged)
			return receipt, err
		}
		response, knownRejection, err := upload(ctx, httpClient, u, o, batch, staged, raw)
		os.Remove(staged)
		if err != nil {
			if knownRejection {
				receipt.Pending = nil
				if saveErr := save(o.Checkpoint, receipt); saveErr != nil {
					return receipt, saveErr
				}
			}
			return receipt, err
		}
		receipt.Completed = end
		receipt.Bytes += n
		receipt.Pending = nil
		receipt.Responses = append(receipt.Responses, response)
		if err = save(o.Checkpoint, receipt); err != nil {
			return receipt, err
		}
		if progress != nil {
			progress(receipt)
		}
	}
	return receipt, nil
}

func upload(ctx context.Context, c *http.Client, base *url.URL, o Options, files []File, path string, raw bool) (json.RawMessage, bool, error) {
	u := *base
	q := u.Query()
	q.Set("owner", o.Owner)
	q.Set("prefix", o.Prefix)
	q.Set("mode", "merge")
	if raw {
		q.Set("path", files[0].Path)
	}
	u.RawQuery = q.Encode()
	f, err := os.Open(path)
	if err != nil {
		return nil, true, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return nil, true, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), f)
	if err != nil {
		return nil, true, err
	}
	req.ContentLength = stat.Size()
	req.Header.Set("Content-Type", "application/zip")
	if raw {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	// Redirecting a capability POST could send file bodies to another host.
	transport := *c
	transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := transport.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("upload interrupted; outcome unknown (checkpoint retained): %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, false, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 408, fmt.Errorf("file import returned HTTP %d; checkpoint retained", resp.StatusCode)
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(body, &result) != nil || result == nil {
		return nil, false, fmt.Errorf("invalid import response; outcome unknown (checkpoint retained)")
	}
	return body, false, nil
}
