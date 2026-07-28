package services

import (
	"fmt"
	"log"
	"time"

	"arcusinvest/internal/config"
	"arcusinvest/internal/models"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Thresholds for what counts as needing attention. These are deliberately
// generous: a notification that fires too eagerly trains people to ignore the
// bell, which costs more than a late reminder.
const (
	RenewalWindow    = 30 * 24 * time.Hour // contract renewal approaching
	ReviewWaiting    = 3 * 24 * time.Hour  // submission sitting unreviewed
	StalledDealAfter = 14 * 24 * time.Hour // open deal with no movement
)

// weekBucket labels a time by its ISO week, for dedupe keys that should repeat
// weekly rather than on every sweep.
//
// This needs ISOWeek: Go's layout language has no week verb, and "2006-W01"
// formats the MONTH, silently turning a weekly reminder into a monthly one.
func weekBucket(t time.Time) string {
	year, week := t.ISOWeek()
	return fmt.Sprintf("%d-W%02d", year, week)
}

// Raise inserts a notification unless the recipient already has one with the
// same dedupe key.
//
// The uniqueness is enforced by the database rather than by a prior SELECT, so
// two replicas sweeping at the same moment cannot both win the race and
// double-notify. A conflict is the expected steady state, not an error.
func Raise(db *gorm.DB, n models.Notification) error {
	res := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&n)
	return res.Error
}

// staffFor returns the users who should hear about a resource. Notifications
// follow the same permission model as everything else: someone who cannot read
// contracts has no business being told about a contract renewal.
func staffFor(db *gorm.DB, canRead func(models.Role) bool) []models.User {
	var users []models.User
	if err := db.Where("is_active = ?", true).Find(&users).Error; err != nil {
		return nil
	}
	out := make([]models.User, 0, len(users))
	for _, u := range users {
		if u.Role != models.RoleStudent && canRead(u.Role) {
			out = append(out, u)
		}
	}
	return out
}

// NotifyApprovalPending tells the approvers eligible to decide a request that it
// is waiting on them.
//
// Recipients are the active users whose role matches the approver role
// snapshotted on the request, filtered by read access to approvals — the same
// permission model the rest of the notifications follow. The requester is
// excluded even if their role would otherwise qualify, so a notification never
// invites someone to approve their own request.
//
// Raised inline rather than by the sweep: an approver who learns about a blocked
// colleague fifteen minutes late is fifteen minutes of someone else's work lost.
func NotifyApprovalPending(db *gorm.DB, req models.ApprovalRequest, canRead func(models.Role, string) bool) {
	approvers := staffFor(db, func(r models.Role) bool {
		return r == req.ApproverRole && canRead(r, "approvals")
	})
	for _, u := range approvers {
		if req.RequesterID != nil && u.ID == *req.RequesterID {
			continue
		}
		if err := Raise(db, models.Notification{
			UserID:     u.ID,
			Kind:       models.NotifyApprovalPending,
			Title:      "Approval needed",
			Body:       fmt.Sprintf("%s requested: %s", req.RequesterName, req.Summary),
			EntityType: "approvals",
			EntityID:   &req.ID,
			DedupeKey:  fmt.Sprintf("%s:%s:%s", models.NotifyApprovalPending, u.ID, req.ID),
		}); err != nil {
			log.Printf("WARN: could not raise approval notification for %s: %v", u.ID, err)
		}
	}
}

// NotifyApprovalDecided closes the loop back to whoever raised the request.
//
// This is what makes revise-and-resubmit work: without it a rejected requester
// has no way to learn the answer, let alone the reason, and the request dies
// silently in a queue nobody revisits.
func NotifyApprovalDecided(db *gorm.DB, req models.ApprovalRequest, decision, reason, approverName string) {
	if req.RequesterID == nil {
		return
	}
	title := "Request approved"
	body := fmt.Sprintf("%s approved: %s", approverName, req.Summary)
	if decision == models.DecisionRejected {
		title = "Request rejected"
		body = fmt.Sprintf("%s rejected: %s — %s", approverName, req.Summary, reason)
	}
	if err := Raise(db, models.Notification{
		UserID:     *req.RequesterID,
		Kind:       models.NotifyApprovalDecided,
		Title:      title,
		Body:       body,
		EntityType: "approvals",
		EntityID:   &req.ID,
		DedupeKey:  fmt.Sprintf("%s:%s:%s", models.NotifyApprovalDecided, *req.RequesterID, req.ID),
	}); err != nil {
		log.Printf("WARN: could not raise approval decision notification: %v", err)
	}
}

// Sweep raises notifications for everything currently needing attention. It is
// idempotent: re-running it raises nothing new until the underlying situation
// changes, because every notification carries a dedupe key derived from the
// thing it is about.
func Sweep(db *gorm.DB, canRead func(models.Role, string) bool) {
	now := time.Now().UTC()

	contractReaders := staffFor(db, func(r models.Role) bool { return canRead(r, "contracts") })
	if len(contractReaders) > 0 {
		var contracts []models.Contract
		// Only live agreements: an expired contract's renewal date is history,
		// and a draft has nothing to renew yet.
		db.Where("renewal_date IS NOT NULL AND renewal_date <= ? AND renewal_date >= ? AND status IN ?",
			now.Add(RenewalWindow), now.Add(-RenewalWindow), []string{"signed", "active", "sent"}).
			Find(&contracts)
		for _, ct := range contracts {
			for _, u := range contractReaders {
				id := ct.ID
				_ = Raise(db, models.Notification{
					UserID: u.ID, Kind: models.NotifyContractRenewal,
					Title: "Contract renewal approaching",
					Body: fmt.Sprintf("%s (%s) renews on %s.",
						ct.Title, ct.AccountName, ct.RenewalDate.Format("2 January 2006")),
					EntityType: "contracts", EntityID: &id,
					// Keyed on the date too, so moving the renewal raises a fresh
					// reminder rather than being silenced by the old one.
					DedupeKey: fmt.Sprintf("%s:%s:%s:%s", models.NotifyContractRenewal, u.ID, ct.ID, ct.RenewalDate.Format("2006-01-02")),
				})
			}
		}
	}

	studentReaders := staffFor(db, func(r models.Role) bool { return canRead(r, "students") })
	if len(studentReaders) > 0 {
		var submissions []models.Submission
		db.Where("status = ? AND created_at <= ?", "submitted", now.Add(-ReviewWaiting)).Find(&submissions)
		for _, s := range submissions {
			for _, u := range studentReaders {
				id := s.ID
				_ = Raise(db, models.Notification{
					UserID: u.ID, Kind: models.NotifySubmissionReview,
					Title:      "Submission awaiting review",
					Body:       fmt.Sprintf("%q has been waiting since %s.", s.Title, s.CreatedAt.Format("2 January")),
					EntityType: "submissions", EntityID: &id,
					DedupeKey: fmt.Sprintf("%s:%s:%s", models.NotifySubmissionReview, u.ID, s.ID),
				})
			}
		}

		var extensions []models.ExtensionRequest
		db.Where("status = ?", "pending").Find(&extensions)
		for _, e := range extensions {
			for _, u := range studentReaders {
				id := e.ID
				_ = Raise(db, models.Notification{
					UserID: u.ID, Kind: models.NotifyExtensionPending,
					Title:      "Extension request pending",
					Body:       fmt.Sprintf("A %s extension to %s is awaiting a decision.", e.ExtensionType, e.RequestedDeadline.Format("2 January 2006")),
					EntityType: "students", EntityID: &id,
					DedupeKey: fmt.Sprintf("%s:%s:%s", models.NotifyExtensionPending, u.ID, e.ID),
				})
			}
		}
	}

	// Stalled deals go to the owner alone. Telling everyone about one person's
	// neglected deal is noise for all but one of them.
	var stalled []models.Opportunity
	db.Where("owner_id IS NOT NULL AND stage NOT IN ? AND updated_at <= ?",
		[]string{"won", "lost"}, now.Add(-StalledDealAfter)).Find(&stalled)
	for _, o := range stalled {
		id := o.ID
		_ = Raise(db, models.Notification{
			UserID: *o.OwnerID, Kind: models.NotifyDealStalled,
			Title: "Deal has gone quiet",
			Body: fmt.Sprintf("%s has had no activity since %s.",
				o.Name, o.UpdatedAt.Format("2 January")),
			EntityType: "opportunities", EntityID: &id,
			// Weekly bucket: a deal that stays stalled should nag again, but not
			// on every sweep.
			DedupeKey: fmt.Sprintf("%s:%s:%s:%s", models.NotifyDealStalled, *o.OwnerID, o.ID, weekBucket(now)),
		})
	}
}

// emailModeFor reads a user's preference, defaulting to digest.
func emailModeFor(db *gorm.DB, userID uuid.UUID) string {
	var pref models.NotificationPreference
	if err := db.Where("user_id = ?", userID).First(&pref).Error; err != nil {
		return models.EmailModeDigest
	}
	if pref.EmailMode == "" {
		return models.EmailModeDigest
	}
	return pref.EmailMode
}

// SendPending emails notifications that have not been emailed yet, honouring
// each recipient's mode. Per-event sends one message per item; digest sends one
// message covering everything outstanding.
//
// Already-read items are skipped: if someone has seen it in the app, mailing
// them about it is pure noise.
func SendPending(db *gorm.DB, cfg *config.Config) {
	if !cfg.MailConfigured() {
		return
	}
	var pending []models.Notification
	if err := db.Where("emailed_at IS NULL AND read_at IS NULL").Order("user_id, created_at").Find(&pending).Error; err != nil {
		return
	}

	byUser := map[uuid.UUID][]models.Notification{}
	for _, n := range pending {
		byUser[n.UserID] = append(byUser[n.UserID], n)
	}

	for userID, items := range byUser {
		var user models.User
		if err := db.First(&user, "id = ? AND is_active = ?", userID, true).Error; err != nil {
			continue
		}
		mode := emailModeFor(db, userID)
		if mode == models.EmailModeNone {
			// Still mark them, or they queue up forever and arrive in a flood if
			// the user later switches the channel back on.
			markEmailed(db, items)
			continue
		}

		var err error
		if mode == models.EmailModePerEvent {
			for _, n := range items {
				if err = SendNotificationEmail(cfg, user.Email, n.Title, n.Body); err != nil {
					break
				}
			}
		} else {
			err = SendNotificationDigestEmail(cfg, user.Email, user.FullName, items)
		}
		if err != nil {
			// Leave them unmarked so the next pass retries rather than losing them.
			log.Printf("notify: could not email %s: %v", user.Email, err)
			continue
		}
		markEmailed(db, items)
	}
}

func markEmailed(db *gorm.DB, items []models.Notification) {
	ids := make([]uuid.UUID, 0, len(items))
	for _, n := range items {
		ids = append(ids, n.ID)
	}
	now := time.Now().UTC()
	db.Model(&models.Notification{}).Where("id IN ?", ids).Update("emailed_at", now)
}

// StartNotificationWorker sweeps for things needing attention and sends the
// resulting mail on a fixed interval.
func StartNotificationWorker(db *gorm.DB, cfg *config.Config, canRead func(models.Role, string) bool, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			Sweep(db, canRead)
			SendPending(db, cfg)
		}
	}()
}
