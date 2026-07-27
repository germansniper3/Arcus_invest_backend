package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"

	"arcusinvest/internal/storage"
)

// The hash is the whole point of storing one: it is what proves a contract was
// not swapped after signature. If saveHashed ever digests something other than
// the exact bytes written to storage — a truncated read, a reused reader, a
// digest taken before the copy — the stored hash silently stops matching the
// stored file and the integrity check becomes theatre.
func TestSaveHashedDigestsExactlyWhatItStores(t *testing.T) {
	store, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	h := Handler{Store: store}

	body := "the quick brown fox jumps over the lazy dog"
	got, err := h.saveHashed("contracts/test.pdf", strings.NewReader(body), int64(len(body)), "application/pdf")
	if err != nil {
		t.Fatalf("saveHashed: %v", err)
	}

	want := sha256.Sum256([]byte(body))
	if got != hex.EncodeToString(want[:]) {
		t.Errorf("hash = %q, want %q", got, hex.EncodeToString(want[:]))
	}

	// And the bytes must actually have landed, not just been hashed in passing.
	rc, err := store.Open("contracts/test.pdf")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	stored, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(stored) != body {
		t.Errorf("stored %q, want %q", stored, body)
	}
}

// An empty upload is rejected before it reaches storage, but the digest of zero
// bytes is still a well-defined value rather than an empty string — a blank
// hash column would read as "not hashed" and defeat the integrity check.
func TestSaveHashedOfEmptyContentIsStillAHash(t *testing.T) {
	store, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	h := Handler{Store: store}

	got, err := h.saveHashed("contracts/empty.pdf", strings.NewReader(""), 0, "application/pdf")
	if err != nil {
		t.Fatalf("saveHashed: %v", err)
	}
	if len(got) != 64 {
		t.Errorf("hash = %q, want 64 hex chars", got)
	}
}
