package services

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

// blankPDF builds a real single-page PDF to stamp. Hand-rolling PDF bytes means
// hand-maintaining an xref table, so the library that reads it also writes it.
func blankPDF(t *testing.T) []byte {
	t.Helper()
	// Helvetica is a core PDF font, so the fixture needs no font files on disk.
	const desc = `{"pages": {"1": {"content": {"text": [
		{"value": "Agreement", "anchor": "center", "font": {"name": "Helvetica", "size": 12}}
	]}}}}`
	var out bytes.Buffer
	if err := api.Create(nil, strings.NewReader(desc), &out, model.NewDefaultConfiguration()); err != nil {
		t.Fatalf("could not build the fixture PDF: %v", err)
	}
	return out.Bytes()
}

// signaturePNG is a small opaque image standing in for a drawn signature.
func signaturePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		img.Set(x, h/2, color.RGBA{0, 0, 0, 255})
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return out.Bytes()
}

// The signed document must be a NEW document: if stamping ever mutated the
// input, the unsigned original would stop being independent evidence and the
// two hashes recorded against a signature would describe the same bytes.
func TestStampSignatureLeavesTheOriginalUntouched(t *testing.T) {
	original := blankPDF(t)
	before := append([]byte(nil), original...)

	signed, err := StampSignature(bytes.NewReader(original), signaturePNG(t, 200, 80),
		SignaturePlacement{Page: 1, X: 0.1, Y: 0.8, WidthFrac: 0.25})
	if err != nil {
		t.Fatalf("StampSignature: %v", err)
	}

	if !bytes.Equal(original, before) {
		t.Error("stamping modified the input PDF; the unsigned original must survive byte for byte")
	}
	if bytes.Equal(signed, original) {
		t.Error("signed output is identical to the input — nothing was stamped")
	}
	// The result has to be a PDF a reader can still open, not just different bytes.
	dims, err := api.PageDims(bytes.NewReader(signed), model.NewDefaultConfiguration())
	if err != nil {
		t.Fatalf("the signed output is not a readable PDF: %v", err)
	}
	if len(dims) != 1 {
		t.Errorf("signed PDF has %d pages, want 1 — stamping must not add or drop pages", len(dims))
	}
}

// A placement past the end of the document is a caller error worth reporting
// clearly, not something to silently clamp onto the last page — a signature on
// the wrong page is worse than a rejected request.
func TestStampSignatureRejectsPageOutsideDocument(t *testing.T) {
	original := blankPDF(t)
	_, err := StampSignature(bytes.NewReader(original), signaturePNG(t, 200, 80),
		SignaturePlacement{Page: 9, X: 0.1, Y: 0.8, WidthFrac: 0.25})
	if err == nil {
		t.Fatal("expected an error for a page outside the document")
	}
	if !strings.Contains(err.Error(), "outside this document") {
		t.Errorf("error = %q, want it to explain the page is outside the document", err)
	}
}

// Anything that is not a decodable PNG must be refused before it reaches the
// PDF, so a corrupt upload fails with a readable message rather than producing
// a signed contract with an invisible or broken stamp.
func TestStampSignatureRejectsNonPNG(t *testing.T) {
	original := blankPDF(t)
	for name, img := range map[string][]byte{
		"empty":   {},
		"garbage": []byte("this is not a png"),
	} {
		if _, err := StampSignature(bytes.NewReader(original), img,
			SignaturePlacement{Page: 1, X: 0.1, Y: 0.8, WidthFrac: 0.25}); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
