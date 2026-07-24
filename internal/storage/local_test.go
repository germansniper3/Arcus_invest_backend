package storage

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalSaveOpenRoundTrip(t *testing.T) {
	dir := t.TempDir()
	l, err := NewLocal(dir)
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	const key = "submissions/abc123.pdf"
	const body = "hello arcus capstone"
	if err := l.Save(key, strings.NewReader(body), int64(len(body)), "application/pdf"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	rc, err := l.Open(key)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != body {
		t.Fatalf("round trip mismatch: got %q want %q", got, body)
	}
}

func TestLocalContainsPathTraversal(t *testing.T) {
	// baseDir is a subdirectory so a successful "../" escape would land in root.
	root := t.TempDir()
	base := filepath.Join(root, "uploads")
	l, err := NewLocal(base)
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	// resolve() neutralises traversal by rooting the key, so these must never
	// write outside base. We assert nothing lands in root (one level up).
	for _, key := range []string{"../escape.txt", "../../escape.txt", "sub/../../escape.txt"} {
		_ = l.Save(key, strings.NewReader("x"), 1, "text/plain")
	}
	if _, err := os.Stat(filepath.Join(root, "escape.txt")); !os.IsNotExist(err) {
		t.Fatalf("path traversal escaped the base dir: %v", err)
	}
}

func TestNewLocalRequiresDir(t *testing.T) {
	if _, err := NewLocal("   "); err == nil {
		t.Fatal("expected error for empty base dir")
	}
}
