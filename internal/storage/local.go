package storage

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Local is a Storage driver that writes objects to the local filesystem under a
// single base directory. Object keys are opaque relative paths; they are joined
// onto the base dir and validated to stay within it.
type Local struct {
	baseDir string
}

// NewLocal creates a Local driver rooted at baseDir, creating the directory
// tree if it does not yet exist.
func NewLocal(baseDir string) (*Local, error) {
	if strings.TrimSpace(baseDir) == "" {
		return nil, fmt.Errorf("storage: base directory is required for the local driver")
	}
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, fmt.Errorf("storage: could not create storage dir %q: %w", baseDir, err)
	}
	return &Local{baseDir: baseDir}, nil
}

// resolve turns an opaque key into an absolute path inside the base dir and
// guards against path-traversal keys ("../"). Callers generate keys themselves,
// but this is defense in depth.
func (l *Local) resolve(key string) (string, error) {
	clean := filepath.Clean("/" + filepath.ToSlash(key))        // force key to be rooted, strip ".."
	full := filepath.Join(l.baseDir, filepath.FromSlash(clean)) // clean starts with "/" so this stays under baseDir
	base, err := filepath.Abs(l.baseDir)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(full)
	if err != nil {
		return "", err
	}
	if abs != base && !strings.HasPrefix(abs, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("storage: invalid key %q", key)
	}
	return abs, nil
}

func (l *Local) Save(key string, r io.Reader, size int64, contentType string) error {
	path, err := l.resolve(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("storage: could not create dir for %q: %w", key, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("storage: could not create file for %q: %w", key, err)
	}
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		return fmt.Errorf("storage: could not write %q: %w", key, err)
	}
	return nil
}

func (l *Local) Open(key string) (io.ReadCloser, error) {
	path, err := l.resolve(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return f, nil
}
