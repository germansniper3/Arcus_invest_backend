package database

import (
	"os"
	"testing"

	"arcusinvest/internal/models"
)

// Integration test: requires a real PostgreSQL via DATABASE_URL. Skipped when
// unset (e.g. local `go test ./...` without a DB); CI provides a Postgres
// service so it runs there.
func TestMigrateAndRoundTrip(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping DB integration test")
	}
	db, err := Connect(dsn)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Every model added to AutoMigrate should have a table we can query.
	quote := models.QuoteRequest{
		Name:    "CI Test Lead",
		Email:   "ci-test@example.com",
		Message: "integration test message",
		Status:  "new",
	}
	if err := db.Create(&quote).Error; err != nil {
		t.Fatalf("create quote: %v", err)
	}
	t.Cleanup(func() { db.Unscoped().Delete(&quote) })

	var got models.QuoteRequest
	if err := db.First(&got, "id = ?", quote.ID).Error; err != nil {
		t.Fatalf("read back quote: %v", err)
	}
	if got.Email != "ci-test@example.com" {
		t.Fatalf("email = %q, want ci-test@example.com", got.Email)
	}

	// New feature tables must exist post-migration.
	for _, m := range []any{&models.ProgressReport{}, &models.ExtensionRequest{}, &models.Submission{}, &models.Broadcast{}} {
		if !db.Migrator().HasTable(m) {
			t.Errorf("expected table for %T to exist after migrate", m)
		}
	}
}
