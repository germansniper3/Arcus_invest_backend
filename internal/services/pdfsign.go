package services

import (
	"bytes"
	"fmt"
	"image"
	_ "image/png" // registers the PNG decoder used by DecodeConfig
	"io"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

// SignaturePlacement describes where a signature image goes on a page.
//
// Coordinates are fractions of the page (0–1) with the origin at the TOP-left,
// because that is how the browser reports a click on a rendered page. PDF user
// space runs from the bottom-left, so Y is flipped during stamping — doing that
// conversion in one place keeps the frontend from having to know about PDF
// coordinate systems.
type SignaturePlacement struct {
	Page      int     // 1-based
	X         float64 // left edge, fraction of page width
	Y         float64 // top edge, fraction of page height
	WidthFrac float64 // signature width as a fraction of page width
}

// maxSignaturePages guards against a placement pointing past the document.
const defaultSignatureWidthFrac = 0.25

// StampSignature draws a PNG signature onto one page of a PDF and returns the
// new document. The input is never modified: the unsigned original stays byte
// for byte as it was, which is what makes it usable as evidence alongside the
// signed copy.
func StampSignature(pdf io.ReadSeeker, signaturePNG []byte, p SignaturePlacement) ([]byte, error) {
	if len(signaturePNG) == 0 {
		return nil, fmt.Errorf("the signature image is empty")
	}

	dims, err := api.PageDims(pdf, model.NewDefaultConfiguration())
	if err != nil {
		return nil, fmt.Errorf("could not read the PDF page layout: %w", err)
	}
	if p.Page < 1 || p.Page > len(dims) {
		return nil, fmt.Errorf("page %d is outside this document (it has %d)", p.Page, len(dims))
	}
	page := dims[p.Page-1]

	// The image's natural size in points, so the requested width can be turned
	// into the absolute scale factor pdfcpu wants. Images are placed at 72dpi,
	// where one pixel is one point.
	cfg, _, err := image.DecodeConfig(bytes.NewReader(signaturePNG))
	if err != nil {
		return nil, fmt.Errorf("the signature is not a readable PNG: %w", err)
	}
	if cfg.Width <= 0 {
		return nil, fmt.Errorf("the signature image has no width")
	}

	widthFrac := p.WidthFrac
	if widthFrac <= 0 {
		widthFrac = defaultSignatureWidthFrac
	}
	targetWidth := widthFrac * page.Width
	scale := targetWidth / float64(cfg.Width)

	// pdfcpu offsets are measured from the anchor, here the page's bottom-left.
	// The stamp is centred on its offset, so half the rendered size is added to
	// turn the caller's top-left placement into a centre point.
	renderedWidth := targetWidth
	renderedHeight := float64(cfg.Height) * scale
	offsetX := p.X*page.Width + renderedWidth/2
	offsetY := (1-p.Y)*page.Height - renderedHeight/2

	// Full parameter names: pdfcpu rejects abbreviations that are ambiguous
	// prefixes, and "sc" matches more than one key.
	desc := fmt.Sprintf("position:bl, offset:%.2f %.2f, scalefactor:%.5f abs, rotation:0, opacity:1",
		offsetX, offsetY, scale)
	wm, err := api.ImageWatermarkForReader(bytes.NewReader(signaturePNG), desc, true /* onTop */, false /* update */, types.POINTS)
	if err != nil {
		return nil, fmt.Errorf("could not prepare the signature stamp: %w", err)
	}

	if _, err := pdf.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("could not rewind the PDF: %w", err)
	}
	var out bytes.Buffer
	if err := api.AddWatermarks(pdf, &out, []string{fmt.Sprint(p.Page)}, wm, model.NewDefaultConfiguration()); err != nil {
		return nil, fmt.Errorf("could not stamp the signature onto the PDF: %w", err)
	}
	return out.Bytes(), nil
}
