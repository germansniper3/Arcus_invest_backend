package handlers

import (
	"encoding/base64"

	"testing"
)

// A browser canvas hands back a full data URL. Accepting only bare base64 would
// mean every caller has to strip the prefix, and one that forgets produces a
// decode failure at signing time rather than at development time.
func TestDecodeSignaturePNGAcceptsDataURLAndBareBase64(t *testing.T) {
	payload := []byte("pretend png bytes")
	encoded := base64.StdEncoding.EncodeToString(payload)

	for name, input := range map[string]string{
		"bare base64": encoded,
		"data url":    "data:image/png;base64," + encoded,
	} {
		got, err := decodeSignaturePNG(input)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", name, err)
			continue
		}
		if string(got) != string(payload) {
			t.Errorf("%s: decoded %q, want %q", name, got, payload)
		}
	}
}

func TestDecodeSignaturePNGRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"empty":            "",
		"whitespace only":  "   ",
		"not base64":       "!!!!not base64!!!!",
		"wrong image type": "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString([]byte("x")),
	}
	for name, input := range cases {
		if _, err := decodeSignaturePNG(input); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// A signature image far larger than a drawn signature is either a mistake or an
// attempt to wedge the stamping step, and must be refused before it reaches the
// PDF library.
func TestDecodeSignaturePNGRejectsOversizeImage(t *testing.T) {
	huge := base64.StdEncoding.EncodeToString(make([]byte, maxSignatureImageBytes+1))
	if _, err := decodeSignaturePNG(huge); err == nil {
		t.Fatal("expected an oversize signature to be rejected")
	}
}

// Contracts accept .doc and .docx, which have no page geometry to stamp. Those
// must be identified as unsignable rather than reaching the PDF library, which
// would fail with something the user cannot act on.
func TestIsPDFDistinguishesStampableDocuments(t *testing.T) {
	stampable := []struct{ contentType, name string }{
		{"application/pdf", "agreement.pdf"},
		{"application/pdf", ""},
		{"", "agreement.pdf"},
		{"", "AGREEMENT.PDF"}, // case must not matter
	}
	for _, tc := range stampable {
		if !isPDF(tc.contentType, tc.name) {
			t.Errorf("isPDF(%q, %q) = false, want true", tc.contentType, tc.name)
		}
	}

	unstampable := []struct{ contentType, name string }{
		{"application/msword", "agreement.doc"},
		{"application/vnd.openxmlformats-officedocument.wordprocessingml.document", "agreement.docx"},
		{"", ""},
		{"text/plain", "notes.txt"},
	}
	for _, tc := range unstampable {
		if isPDF(tc.contentType, tc.name) {
			t.Errorf("isPDF(%q, %q) = true, want false", tc.contentType, tc.name)
		}
	}
}

// The guard in AdminUpdateContract compares against this exact literal. If the
// status is ever renamed in the map without updating the guard, `signed`
// silently becomes settable from the dropdown again — which is precisely the
// hole the signing endpoint exists to close.
func TestSignedRemainsTheStatusTheUpdateGuardChecks(t *testing.T) {
	if !validContractStatuses["signed"] {
		t.Fatal(`"signed" is no longer a valid contract status — the guard in AdminUpdateContract now checks a status that cannot occur`)
	}
}
