// Package storage provides a pluggable file-storage abstraction for uploaded
// submission files. The local-disk driver is used today; an S3 driver is
// stubbed out so it can be dropped in for hosting without touching callers.
package storage

import (
	"fmt"
	"io"

	"arcusinvest/internal/config"
)

// Storage is the minimal interface every driver implements. Keys are opaque,
// server-generated relative paths (e.g. "submissions/<uuid>.pdf"); callers must
// never build a key from a client-supplied filename.
type Storage interface {
	// Save persists the contents of r under key. size and contentType are
	// advisory (the local driver ignores them; the S3 driver will use them).
	Save(key string, r io.Reader, size int64, contentType string) error
	// Open returns a readable stream for the object stored under key. The
	// caller is responsible for closing it.
	Open(key string) (io.ReadCloser, error)
}

// New builds the storage driver selected by cfg.StorageDriver. It fails fast at
// startup for unimplemented or unknown drivers so misconfiguration surfaces
// before the server accepts traffic.
func New(cfg *config.Config) (Storage, error) {
	switch cfg.StorageDriver {
	case "local", "":
		return NewLocal(cfg.StorageDir)
	case "s3":
		return nil, fmt.Errorf("s3 storage driver not yet implemented — set STORAGE_DRIVER=local")
	default:
		return nil, fmt.Errorf("unknown STORAGE_DRIVER %q — set STORAGE_DRIVER=local", cfg.StorageDriver)
	}
}
