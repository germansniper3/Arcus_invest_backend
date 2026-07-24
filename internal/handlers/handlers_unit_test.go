package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

func TestValidMilestoneStatus(t *testing.T) {
	ok := []string{"pending", "in_progress", "pending_review", "completed"}
	for _, s := range ok {
		if !validMilestoneStatus(s) {
			t.Errorf("expected %q to be valid", s)
		}
	}
	bad := []string{"", "banana", "done", "COMPLETED", "reviewed"}
	for _, s := range bad {
		if validMilestoneStatus(s) {
			t.Errorf("expected %q to be invalid", s)
		}
	}
}

func TestAllowedSubmissionExts(t *testing.T) {
	for _, ext := range []string{".pdf", ".docx", ".png", ".zip", ".csv"} {
		if !allowedSubmissionExts[ext] {
			t.Errorf("expected %q to be allowed", ext)
		}
	}
	for _, ext := range []string{".exe", ".sh", ".js", ".", ""} {
		if allowedSubmissionExts[ext] {
			t.Errorf("expected %q to be rejected", ext)
		}
	}
}

func TestMaxSubmissionSizeIs15MB(t *testing.T) {
	if maxSubmissionSize != 15*1024*1024 {
		t.Fatalf("maxSubmissionSize = %d, want 15MB", maxSubmissionSize)
	}
}

// Health needs no database, so we can exercise the real handler over httptest.
func TestHealthEndpoint(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	h := Handler{}
	if err := h.Health(c); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "arcusinvest-api") {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}
