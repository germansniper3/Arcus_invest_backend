package main

import (
	"arcusinvest/internal/authz"
	"arcusinvest/internal/config"
	"arcusinvest/internal/database"
	"arcusinvest/internal/handlers"
	appmw "arcusinvest/internal/middleware"
	"arcusinvest/internal/models"
	"arcusinvest/internal/seed"
	"arcusinvest/internal/services"
	"arcusinvest/internal/storage"
	"log"
	"net"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"golang.org/x/time/rate"
	"gorm.io/gorm"
)

// rateLimiter builds a per-IP in-memory rate limiter allowing perMinute
// requests per minute (with a matching burst) on the routes it is attached to.
func rateLimiter(perMinute int) echo.MiddlewareFunc {
	store := middleware.NewRateLimiterMemoryStoreWithConfig(middleware.RateLimiterMemoryStoreConfig{
		Rate:      rate.Limit(float64(perMinute) / 60.0),
		Burst:     perMinute,
		ExpiresIn: 3 * time.Minute,
	})
	return middleware.RateLimiter(store)
}

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	db, err := database.Connect(cfg.DatabaseURL)
	if err != nil {
		log.Fatal(err)
	}
	if err := database.Migrate(db); err != nil {
		log.Fatal(err)
	}
	// Carries the pre-ledger products.stock values in as opening balances. It
	// skips any product that already has movements, so it is safe on every
	// boot; failing here would leave stock reading zero everywhere, which is a
	// worse lie than not starting.
	if err := handlers.BackfillStockOpeningBalances(db); err != nil {
		log.Fatal(err)
	}
	if config.LegacyTokenTTLSet() {
		log.Printf("WARN: JWT_TTL_HOURS is set but no longer used. Access tokens now last "+
			"ACCESS_TOKEN_TTL_MINUTES (%s) and sessions are carried by refresh tokens for "+
			"REFRESH_TOKEN_TTL_DAYS (%s). Remove JWT_TTL_HOURS to silence this.",
			config.AccessTokenTTL(), config.RefreshTokenTTL())
	}
	if err := seed.Roles(db); err != nil {
		log.Fatal(err)
	}
	if err := authz.LoadFromDB(db); err != nil {
		log.Printf("WARN: authz.LoadFromDB failed, using hardcoded fallback: %v", err)
	}
	// Background goroutine re-reads grants every 30s for multi-replica
	// coherence. Sub-30s consistency is not guaranteed across replicas.
	// See also: numReplicas in railway.json.
	authz.StartCacheRefresher(db, 30*time.Second)
	if err := seed.Admin(db, cfg); err != nil {
		log.Fatal(err)
	}
	// Build the file-storage driver (creates the storage dir for the local
	// driver). Fail fast so a bad STORAGE_DRIVER surfaces at startup.
	store, err := storage.New(cfg)
	if err != nil {
		log.Fatal(err)
	}

	// Contracts uploaded before versioning existed have no version row. Give
	// them one so their history is not blank. Logged rather than fatal: a
	// storage hiccup must not stop the API from booting.
	if err := handlers.BackfillDocumentVersions(db, store); err != nil {
		log.Printf("WARN: document version backfill failed: %v", err)
	}

	// Raise notifications for things needing attention, and send the resulting
	// mail. Reads permissions through authz so a role that cannot see contracts
	// is never told about a contract renewal.
	services.StartNotificationWorker(db, cfg, func(role models.Role, resource string) bool {
		return authz.Can(role, authz.Resource(resource), authz.ActionRead)
	}, 15*time.Minute)

	h := handlers.Handler{DB: db, Cfg: cfg, Store: store}
	e := buildRouter(h, cfg, db)

	listener, err := net.Listen("tcp4", "0.0.0.0:"+cfg.Port)
	if err != nil {
		log.Fatal(err)
	}
	e.Listener = listener
	log.Fatal(e.Start(""))
}

// buildRouter registers every route. It is separate from main so tests can walk
// the real route table — see TestEveryAdminRouteIsPermissioned, which is what
// keeps the default-deny guarantee honest as routes are added.
func buildRouter(h handlers.Handler, cfg *config.Config, db *gorm.DB) *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	// Identify clients by the direct socket address so rate limiting cannot be
	// bypassed with a spoofed X-Forwarded-For header. If the API is deployed
	// behind a trusted reverse proxy, switch to echo.ExtractIPFromXFFHeader
	// with that proxy's ranges configured as trusted.
	e.IPExtractor = echo.ExtractIPDirect()
	e.Use(middleware.Recover())
	e.Use(middleware.Logger())
	e.Use(middleware.CORSWithConfig(middleware.CORSConfig{
		AllowOrigins: cfg.CORSOrigins,
		AllowMethods: []string{echo.GET, echo.POST, echo.PUT, echo.PATCH, echo.DELETE, echo.OPTIONS},
		AllowHeaders: []string{echo.HeaderOrigin, echo.HeaderContentType, echo.HeaderAccept, echo.HeaderAuthorization},
		// Required for the refresh cookie to cross from the frontend origin to
		// this one. Safe only because AllowOrigins is an explicit allowlist —
		// browsers refuse credentials against a wildcard, and so should we.
		AllowCredentials: true,
	}))

	api := e.Group("/api/v1")

	// Per-IP rate limiters for unauthenticated POST endpoints. Each endpoint
	// gets its own store so their budgets do not compete with one another.

	// ── Public ──────────────────────────────────────────────────────────────
	api.GET("/health", h.Health)
	api.POST("/auth/login", h.Login, rateLimiter(10))
	// Refresh and logout are public because they authenticate with the refresh
	// cookie, not a bearer token — by the time refresh is needed, the access
	// token has usually already expired.
	api.POST("/auth/refresh", h.Refresh, rateLimiter(60))
	api.POST("/auth/logout", h.Logout, rateLimiter(30))
	// Tighter budgets than the other public POSTs. The limiter is per-process
	// and in-memory, so with more than one replica it is a speed bump rather
	// than the control — the real anti-enumeration defence is that the response
	// never varies.
	api.POST("/auth/forgot-password", h.ForgotPassword, rateLimiter(5))
	api.POST("/auth/reset-password", h.ResetPassword, rateLimiter(10))
	api.POST("/enrollments", h.CreateEnrollment, rateLimiter(20))
	api.POST("/quotes", h.CreateQuote, rateLimiter(20))
	api.POST("/chat", h.Chat, rateLimiter(20))

	// Public events
	api.GET("/events", h.ListPublicEvents)
	api.GET("/events/:slug", h.GetPublicEvent)
	api.POST("/events/:id/reserve", h.CreateReservation, rateLimiter(20))

	// Public gallery
	api.GET("/gallery", h.ListPublicGallery)
	api.GET("/gallery/images/:name", h.ServeGalleryImage)

	// Public products
	api.GET("/products", h.ListPublicProducts)
	api.GET("/products/images/:name", h.ServeProductImage)
	api.GET("/products/:slug", h.GetPublicProduct)

	// Invitation claim (public — unauthenticated student sets password)
	api.GET("/invitations/:token", h.PreviewInvitation)
	api.POST("/invitations/claim", h.ClaimInvitation, rateLimiter(20))

	// ── Protected (any authenticated user) ──────────────────────────────────
	protected := api.Group("")
	protected.Use(appmw.Auth(cfg, db))
	protected.GET("/auth/me", h.Me)
	// Requires a live token: ending every session everywhere is a decision only
	// the account holder should be able to make.
	protected.POST("/auth/logout-everywhere", h.LogoutEverywhere)
	protected.POST("/chat/authenticated", h.Chat)

	// ── Student Hub ──────────────────────────────────────────────────────────
	student := protected.Group("/student")
	student.Use(appmw.RequireRoles(models.RoleStudent))
	student.GET("/dashboard", h.StudentDashboard)
	student.PATCH("/capstone", h.UpdateCapstone)
	student.PATCH("/milestones/:mid", h.UpdateMilestone)
	student.POST("/comments", h.PostComment)
	student.POST("/progress-reports", h.CreateProgressReport)
	student.POST("/extensions", h.CreateExtensionRequest)
	student.POST("/submissions", h.CreateSubmission)
	student.GET("/submissions/:id/file", h.StudentDownloadSubmission)

	// ── Admin ────────────────────────────────────────────────────────────────
	admin := protected.Group("/admin")
	// Coarse gate: students never reach the admin surface at all. All other
	// roles (built-in staff + custom) pass through to RequirePermission.
	admin.Use(appmw.RejectStudents)
	// Fine gate: per-resource permissions, default-deny. This is the single
	// source of truth (internal/authz) — do NOT add per-route RequireRoles, or
	// the two will drift.
	admin.Use(appmw.RequirePermission)
	// Record every successful admin mutation to the immutable audit trail.
	admin.Use(h.AuditMutations)

	// Dashboard metrics
	admin.GET("/metrics", h.Metrics)

	// Audit trail
	admin.GET("/audit-logs", h.AdminListAuditLogs)

	// Quotes / contact
	admin.GET("/quotes", h.ListQuotes)
	admin.PATCH("/quotes/:id", h.AdminUpdateQuote)
	admin.POST("/quotes/:id/convert", h.AdminConvertQuote)

	// Staff directory (owner picker for the pipeline)
	admin.GET("/staff", h.AdminListStaff)

	// Enrollments
	admin.GET("/enrollments", h.ListEnrollments)
	admin.POST("/enrollments", h.AdminCreateEnrollment)
	admin.PATCH("/enrollments/:id", h.UpdateEnrollment)
	admin.POST("/enrollments/:id/invite", h.GenerateInvite)

	// Outbound email diagnostics
	admin.GET("/email/status", h.AdminEmailStatus)
	admin.POST("/email/test", h.AdminSendTestEmail)

	// Students (hub portal — admin-only visibility)
	admin.GET("/students", h.ListStudents)
	admin.GET("/students/:id", h.AdminStudentDetail)
	admin.POST("/students/:id/comments", h.AdminPostComment)
	admin.PATCH("/students/:id/milestones/:mid", h.AdminUpdateMilestone)
	admin.PATCH("/progress-reports/:id", h.AdminRespondProgressReport)
	admin.PATCH("/extensions/:id", h.AdminRespondExtension)
	admin.GET("/submissions/:id/file", h.AdminDownloadSubmission)
	admin.PATCH("/submissions/:id", h.AdminReviewSubmission)

	// Events
	admin.GET("/events", h.AdminListEvents)
	admin.POST("/events", h.AdminCreateEvent)
	admin.PUT("/events/:id", h.AdminUpdateEvent)
	admin.DELETE("/events/:id", h.AdminDeleteEvent)
	admin.GET("/events/:id/reservations", h.AdminListReservations)
	admin.PATCH("/reservations/:rid/approve", h.AdminApproveReservation)
	admin.POST("/events/:id/broadcast", h.AdminBroadcast)

	// Products
	admin.GET("/products", h.AdminListProducts)
	admin.POST("/products", h.AdminCreateProduct)
	admin.POST("/products/image", h.UploadProductImage)
	admin.PUT("/products/:id", h.AdminUpdateProduct)
	admin.DELETE("/products/:id", h.AdminDeleteProduct)
	// The stock ledger. Quantity on hand is the sum of these rows; nothing
	// writes a balance directly.
	admin.GET("/products/:id/stock-movements", h.AdminListStockMovements)
	admin.POST("/products/:id/stock-movements", h.AdminCreateStockMovement)

	// Gallery (public "our work" showcase)
	admin.GET("/gallery", h.AdminListGallery)
	admin.POST("/gallery", h.AdminCreateGalleryItem)
	admin.POST("/gallery/image", h.UploadGalleryImage)
	admin.PUT("/gallery/:id", h.AdminUpdateGalleryItem)
	admin.DELETE("/gallery/:id", h.AdminDeleteGalleryItem)

	// Opportunities (B2B sales pipeline)
	admin.GET("/opportunities", h.AdminListOpportunities)
	admin.GET("/opportunities/forecast", h.AdminPipelineForecast)
	admin.GET("/accounts", h.AdminAccountsIndex)
	admin.GET("/accounts/:name/recommendations", h.AdminAccountRecommendations)
	admin.GET("/accounts/:name/payments", h.AdminAccountPayments)

	// Receivables and exports. Each export sits under the resource it exposes,
	// so it inherits that resource's permission rather than needing its own.
	admin.GET("/receivables", h.AdminReceivables)
	admin.GET("/receivables/export", h.AdminExportReceivablesCSV)
	admin.GET("/opportunities/export", h.AdminExportPipelineCSV)
	admin.GET("/payments/export", h.AdminExportPaymentsCSV)

	// Payables — the counterpart to the receivable side. Recording a supplier
	// invoice and settling it are separately gated; see ApprovalExpenseRecord.
	admin.GET("/expenses", h.AdminListExpenses)
	admin.POST("/expenses", h.AdminCreateExpense)
	admin.PUT("/expenses/:id", h.AdminUpdateExpense)
	admin.DELETE("/expenses/:id", h.AdminDeleteExpense)
	admin.POST("/expenses/:id/settlements", h.AdminSettleExpense)
	admin.GET("/payables", h.AdminPayables)
	admin.GET("/payables/export", h.AdminExportPayablesCSV)
	// The combined cash position: owed to us, owed by us, net.
	admin.GET("/position", h.AdminPosition)

	// Walk-in selling. Deliberately separate from the deal pipeline — see the
	// note on models.CounterSale.
	admin.GET("/till-sessions", h.AdminListTillSessions)
	admin.POST("/till-sessions", h.AdminOpenTill)
	admin.GET("/till-sessions/:id", h.AdminTillSummary)
	admin.PATCH("/till-sessions/:id/close", h.AdminCloseTill)
	admin.GET("/counter-sales", h.AdminListCounterSales)
	admin.POST("/counter-sales", h.AdminCreateCounterSale)

	// Notifications (personal inbox — every query is scoped to the caller)
	admin.GET("/notifications", h.AdminListNotifications)
	// PATCH, not POST: marking read is an update, and the notifications grant is
	// read+update deliberately — nobody creates or deletes their own inbox items.
	admin.PATCH("/notifications/read-all", h.AdminMarkAllNotificationsRead)
	admin.PATCH("/notifications/:id/read", h.AdminMarkNotificationRead)
	admin.GET("/notifications/preferences", h.AdminGetNotificationPreference)
	admin.PUT("/notifications/preferences", h.AdminSetNotificationPreference)

	// Approvals (maker-checker queue for gated actions)
	admin.GET("/approvals", h.AdminListApprovals)
	admin.GET("/approvals/:id", h.AdminGetApproval)
	// PATCH, not POST: deciding is an update to an existing request. A POST would
	// need a create grant that approvers must not have, and PATCH .../approve
	// also lands in AuditLog as "approve" rather than a generic "update".
	admin.PATCH("/approvals/:id/approve", h.AdminApproveRequest)
	admin.PATCH("/approvals/:id/reject", h.AdminRejectRequest)
	// Resubmitting genuinely creates a new request, so this one is a POST.
	admin.POST("/approvals/:id/resubmit", h.AdminResubmitRequest)
	// The thresholds share the approvals resource: anyone able to rewrite them
	// can neutralise the control, so this must not be the softer permission.
	admin.GET("/approval-rules", h.AdminListApprovalRules)
	admin.POST("/approval-rules", h.AdminCreateApprovalRule)
	admin.PATCH("/approval-rules/:id", h.AdminUpdateApprovalRule)

	// Contracts (repository + renewal tracking)
	admin.GET("/contracts", h.AdminListContracts)
	admin.POST("/contracts", h.AdminCreateContract)
	admin.PUT("/contracts/:id", h.AdminUpdateContract)
	admin.DELETE("/contracts/:id", h.AdminDeleteContract)
	admin.POST("/contracts/:id/file", h.UploadContractFile)
	admin.GET("/contracts/:id/file", h.AdminDownloadContract)
	admin.GET("/contracts/:id/versions", h.AdminListContractVersions)
	admin.GET("/contracts/:id/versions/:versionId/file", h.AdminDownloadContractVersion)
	admin.GET("/contracts/:id/access-log", h.AdminListContractAccessLog)
	admin.POST("/contracts/:id/sign", h.AdminSignContract)
	admin.GET("/contracts/:id/signatures", h.AdminListContractSignatures)
	// The caller's own saved signature. Grouped under contracts because that is
	// the only thing it is used for, and so it inherits contract permissions.
	admin.GET("/contracts/my-signature", h.AdminGetMySignature)
	admin.DELETE("/contracts/my-signature", h.AdminDeleteMySignature)
	admin.POST("/opportunities", h.AdminCreateOpportunity)
	admin.PUT("/opportunities/:id", h.AdminUpdateOpportunity)
	admin.PATCH("/opportunities/:id/invoiced", h.AdminMarkInvoiced)
	admin.DELETE("/opportunities/:id", h.AdminDeleteOpportunity)
	admin.GET("/opportunities/:id/activities", h.AdminListActivities)
	admin.POST("/opportunities/:id/activities", h.AdminCreateActivity)
	admin.GET("/opportunities/:id/payments", h.AdminListPayments)
	admin.POST("/opportunities/:id/payments", h.AdminCreatePayment)
	admin.DELETE("/payments/:id", h.AdminDeletePayment)

	// User management (authz grants this to super_admin + admin only)
	admin.GET("/users", h.AdminListUsers)
	admin.POST("/users", h.CreateUser)
	admin.PATCH("/users/:id", h.AdminUpdateUser)
	admin.DELETE("/users/:id", h.AdminDeleteUser)

	// Role management (authz.ResRoles — restricted to super_admin)
	admin.GET("/roles", h.ListRoles)
	admin.POST("/roles", h.CreateRole)
	admin.PATCH("/roles/:id", h.UpdateRole)
	admin.DELETE("/roles/:id", h.DeleteRole)

	return e
}
