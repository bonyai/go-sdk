package bonya

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// DefaultReadFileLimit is ReadFile's size cap when none is given.
const DefaultReadFileLimit = 64 << 20

const octetStream = "application/octet-stream"

// FileKind is the type of a filesystem entry.
type FileKind string

const (
	FileKindFile      FileKind = "file"
	FileKindDirectory FileKind = "directory"
	FileKindSymlink   FileKind = "symlink"
	FileKindOther     FileKind = "other"
)

// FileInfo describes a file or directory in a sandbox.
type FileInfo struct {
	Path string
	Name string
	Kind FileKind
	Size int64
	// Mode is the Unix mode as the sandbox reports it.
	Mode       uint32
	ModifiedAt time.Time
}

// IsDir reports whether the entry is a directory.
func (f *FileInfo) IsDir() bool { return f.Kind == FileKindDirectory }

type fileInfoWire struct {
	Path                string `json:"path"`
	Name                string `json:"name"`
	Kind                string `json:"kind"`
	Size                uint64 `json:"size"`
	Mode                uint32 `json:"mode"`
	ModifiedAtUnixNanos int64  `json:"modified_at_unix_nanos"`
}

func (w fileInfoWire) info() FileInfo {
	return FileInfo{
		Path:       w.Path,
		Name:       w.Name,
		Kind:       FileKind(w.Kind),
		Size:       int64(w.Size),
		Mode:       w.Mode,
		ModifiedAt: unixNanos(w.ModifiedAtUnixNanos),
	}
}

func pathValues(path string) (url.Values, error) {
	if path == "" {
		return nil, invalidArgument("path is required")
	}
	return url.Values{"path": {path}}, nil
}

// OpenFile streams a file from the sandbox. Close the reader when done. A
// transfer that breaks midway fails the read with an *Error (ErrUnavailable).
func (s *Sandbox) OpenFile(ctx context.Context, path string) (io.ReadCloser, error) {
	query, err := pathValues(path)
	if err != nil {
		return nil, err
	}
	resp, err := s.edgeDo(ctx, &request{
		method: http.MethodGet,
		path:   sandboxPath(s.ID, "fs", "read"),
		query:  query,
		stream: true,
	})
	if err != nil {
		return nil, err
	}
	return &transferBody{ReadCloser: resp.Body, sandboxID: s.ID}, nil
}

// transferBody turns a broken download into an *Error.
type transferBody struct {
	io.ReadCloser
	sandboxID string
}

func (b *transferBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		var e *Error
		if errors.As(err, &e) {
			return n, err
		}
		return n, &Error{Kind: ErrUnavailable, Message: "file transfer interrupted: " + err.Error(), SandboxID: b.sandboxID, cause: err}
	}
	return n, err
}

// ReadFile reads a whole file. Files larger than maxBytes fail with
// ErrTooLarge; maxBytes <= 0 means DefaultReadFileLimit.
func (s *Sandbox) ReadFile(ctx context.Context, path string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultReadFileLimit
	}
	r, err := s.OpenFile(ctx, path)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, &Error{Kind: ErrTooLarge, Message: "file is larger than " + strconv.FormatInt(maxBytes, 10) + " bytes", SandboxID: s.ID}
	}
	return data, nil
}

// WriteFile replaces a file in the sandbox with data.
func (s *Sandbox) WriteFile(ctx context.Context, path string, data []byte) (int64, error) {
	return s.writeFile(ctx, path, func() (io.Reader, error) { return bytes.NewReader(data), nil }, false)
}

// WriteFileFrom replaces a file in the sandbox with everything read from r.
// r is read once, so the upload is not resent after it has started; use
// WriteFile or Upload for a retryable upload.
func (s *Sandbox) WriteFileFrom(ctx context.Context, path string, r io.Reader) (int64, error) {
	used := false
	return s.writeFile(ctx, path, func() (io.Reader, error) {
		if used {
			return nil, errors.New("the reader was already consumed")
		}
		used = true
		return r, nil
	}, true)
}

// Upload copies a local file to path in the sandbox. The local file is
// opened again for every attempt, so the upload is retried safely.
func (s *Sandbox) Upload(ctx context.Context, localPath, remotePath string) (int64, error) {
	info, err := os.Stat(localPath)
	if err != nil {
		return 0, &Error{Kind: ErrInvalidRequest, Message: "open local file: " + err.Error(), SandboxID: s.ID, cause: err}
	}
	if !info.Mode().IsRegular() {
		return 0, invalidArgument(localPath + " is not a regular file")
	}
	return s.writeFile(ctx, remotePath, func() (io.Reader, error) {
		// The HTTP transport closes the file after sending it.
		return os.Open(localPath)
	}, false)
}

func (s *Sandbox) writeFile(ctx context.Context, path string, body func() (io.Reader, error), singleShot bool) (int64, error) {
	query, err := pathValues(path)
	if err != nil {
		return 0, err
	}
	var out struct {
		BytesWritten uint64 `json:"bytes_written"`
	}
	err = s.edgeJSON(ctx, &request{
		method:         http.MethodPut,
		path:           sandboxPath(s.ID, "fs", "write"),
		query:          query,
		body:           body,
		contentType:    octetStream,
		idempotencyKey: newIdempotencyKey(),
		singleShot:     singleShot,
		// A transfer is bounded by its context, not the client default.
		stream: true,
	}, &out)
	if err != nil {
		return 0, err
	}
	return int64(out.BytesWritten), nil
}

// Download copies a file from the sandbox to localPath. The local file is
// replaced atomically: it is written next to localPath and renamed into
// place only when complete.
func (s *Sandbox) Download(ctx context.Context, remotePath, localPath string) (int64, error) {
	r, err := s.OpenFile(ctx, remotePath)
	if err != nil {
		return 0, err
	}
	defer r.Close()

	localErr := func(msg string, err error) error {
		return &Error{Kind: ErrInvalidRequest, Message: msg + ": " + err.Error(), SandboxID: s.ID, cause: err}
	}
	mode := os.FileMode(0o644)
	if existing, err := os.Stat(localPath); err == nil {
		mode = existing.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(localPath), "."+filepath.Base(localPath)+".bonya-*")
	if err != nil {
		return 0, localErr("create local file", err)
	}
	tmpName := tmp.Name()
	done := false
	defer func() {
		if !done {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	n, err := io.Copy(tmp, r)
	if err != nil {
		var e *Error
		if errors.As(err, &e) {
			return 0, err
		}
		return 0, localErr("write local file", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		return 0, localErr("set local file mode", err)
	}
	if err := tmp.Sync(); err != nil {
		return 0, localErr("sync local file", err)
	}
	if err := tmp.Close(); err != nil {
		return 0, localErr("close local file", err)
	}
	if err := os.Rename(tmpName, localPath); err != nil {
		return 0, localErr("replace local file", err)
	}
	done = true
	return n, nil
}

// ListDir lists a directory.
func (s *Sandbox) ListDir(ctx context.Context, path string) ([]FileInfo, error) {
	query, err := pathValues(path)
	if err != nil {
		return nil, err
	}
	var out struct {
		Files []fileInfoWire `json:"files"`
	}
	if err := s.edgeJSON(ctx, &request{method: http.MethodGet, path: sandboxPath(s.ID, "fs", "list"), query: query}, &out); err != nil {
		return nil, err
	}
	files := make([]FileInfo, 0, len(out.Files))
	for _, w := range out.Files {
		files = append(files, w.info())
	}
	return files, nil
}

// Stat describes one path. A missing path is ErrNotFound.
func (s *Sandbox) Stat(ctx context.Context, path string) (*FileInfo, error) {
	query, err := pathValues(path)
	if err != nil {
		return nil, err
	}
	var out struct {
		File fileInfoWire `json:"file"`
	}
	if err := s.edgeJSON(ctx, &request{method: http.MethodGet, path: sandboxPath(s.ID, "fs", "stat"), query: query}, &out); err != nil {
		return nil, err
	}
	info := out.File.info()
	return &info, nil
}

// Mkdir creates a directory.
func (s *Sandbox) Mkdir(ctx context.Context, path string) error {
	if path == "" {
		return invalidArgument("path is required")
	}
	return s.edgeJSON(ctx, &request{
		method: http.MethodPost,
		path:   sandboxPath(s.ID, "fs", "mkdir"),
		json: struct {
			Path string `json:"path"`
		}{path},
		idempotencyKey: newIdempotencyKey(),
	}, nil)
}

// Remove deletes a file or an empty directory.
func (s *Sandbox) Remove(ctx context.Context, path string) error {
	return s.remove(ctx, path, false)
}

// RemoveAll deletes a path and everything below it.
func (s *Sandbox) RemoveAll(ctx context.Context, path string) error {
	return s.remove(ctx, path, true)
}

func (s *Sandbox) remove(ctx context.Context, path string, recursive bool) error {
	query, err := pathValues(path)
	if err != nil {
		return err
	}
	if recursive {
		query.Set("recursive", "true")
	}
	return s.edgeJSON(ctx, &request{
		method:         http.MethodDelete,
		path:           sandboxPath(s.ID, "fs"),
		query:          query,
		idempotencyKey: newIdempotencyKey(),
	}, nil)
}

// Rename moves a file or directory from one path to another.
func (s *Sandbox) Rename(ctx context.Context, from, to string) error {
	if from == "" || to == "" {
		return invalidArgument("from and to are required")
	}
	return s.edgeJSON(ctx, &request{
		method: http.MethodPost,
		path:   sandboxPath(s.ID, "fs", "rename"),
		json: struct {
			From string `json:"from"`
			To   string `json:"to"`
		}{from, to},
		idempotencyKey: newIdempotencyKey(),
	}, nil)
}
