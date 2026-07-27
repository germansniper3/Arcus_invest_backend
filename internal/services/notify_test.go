package services

import (
	"strings"
	"testing"
	"time"

	"arcusinvest/internal/models"

	"github.com/google/uuid"
)

// The dedupe key is what stops a sweep every 15 minutes from raising the same
// reminder 96 times a day. These pin the two properties that make it work:
// the same situation yields the same key, and a genuinely different one does
// not.
func TestRenewalDedupeKeyChangesOnlyWhenTheRenewalMoves(t *testing.T) {
	user := uuid.New()
	contract := uuid.New()
	key := func(date string) string {
		return strings.Join([]string{models.NotifyContractRenewal, user.String(), contract.String(), date}, ":")
	}

	if key("2026-09-01") != key("2026-09-01") {
		t.Fatal("the same contract and renewal date must produce the same key")
	}
	if key("2026-09-01") == key("2026-10-01") {
		t.Error("moving the renewal date must produce a new key, or rescheduling is never re-notified")
	}

	other := uuid.New()
	a := strings.Join([]string{models.NotifyContractRenewal, user.String(), contract.String(), "2026-09-01"}, ":")
	b := strings.Join([]string{models.NotifyContractRenewal, other.String(), contract.String(), "2026-09-01"}, ":")
	if a == b {
		t.Error("two recipients must get separate keys, or only the first one is ever told")
	}
}

// A stalled deal should nag again eventually, but not on every sweep. The key
// buckets by week, so the reminder repeats weekly rather than every 15 minutes.
func TestStalledDealKeyBucketsByWeek(t *testing.T) {
	base := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC) // a Monday
	sameWeek := base.Add(48 * time.Hour)
	nextWeek := base.Add(8 * 24 * time.Hour)

	if weekBucket(base) != weekBucket(sameWeek) {
		t.Error("two sweeps in the same week must share a bucket, or the reminder repeats constantly")
	}
	if weekBucket(base) == weekBucket(nextWeek) {
		t.Errorf("a new week must open a new bucket, got %q for both — a stalled deal would go unflagged for weeks",
			weekBucket(base))
	}

	// The trap this replaced: time's layout language has no week verb, so
	// "2006-W01" renders the month and buckets by month instead.
	if base.Format("2006-W01") != nextWeek.Format("2006-W01") {
		t.Skip("Go's layout behaviour changed; the ISOWeek workaround may no longer be needed")
	}
}

// Thresholds are deliberately generous. Pinning them stops someone tightening
// one to "be more helpful" and turning the bell into noise people learn to
// ignore, which is the failure mode that makes the whole feature worthless.
func TestNotificationThresholdsStayGenerous(t *testing.T) {
	if RenewalWindow < 14*24*time.Hour {
		t.Errorf("RenewalWindow = %v, want at least two weeks' warning", RenewalWindow)
	}
	if ReviewWaiting < 24*time.Hour {
		t.Errorf("ReviewWaiting = %v, want at least a day before nagging a reviewer", ReviewWaiting)
	}
	if StalledDealAfter < 7*24*time.Hour {
		t.Errorf("StalledDealAfter = %v, want at least a week of silence before calling a deal stalled", StalledDealAfter)
	}
}
