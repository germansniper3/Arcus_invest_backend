package main

import (
	"arcusinvest/internal/config"
	"arcusinvest/internal/database"
	"arcusinvest/internal/handlers"
	appmw "arcusinvest/internal/middleware"
	"arcusinvest/internal/models"
	"arcusinvest/internal/seed"
	"arcusinvest/internal/storage"
	"log"
	"net"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"golang.org/x/time/rate"
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
	if err := seed.Admin(db, cfg); err != nil {
		log.Fatal(err)
	}
	// Build the file-storage driver (creates the storage dir for the local
	// driver). Fail fast so a bad STORAGE_DRIVER surfaces at startup.
	store, err := storage.New(cfg)
	if err != nil {
		log.Fatal(err)
	}

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
	}))

	h := handlers.Handler{DB: db, Cfg: cfg, Store: store}
	api := e.Group("/api/v1")

	// Per-IP rate limiters for unauthenticated POST endpoints. Each endpoint
	// gets its own store so their budgets do not compete with one another.

	// ── Public ──────────────────────────────────────────────────────────────
	api.GET("/health", h.Health)
	api.POST("/auth/login", h.Login, rateLimiter(10))
	api.POST("/enrollments", h.CreateEnrollment, rateLimiter(20))
	api.POST("/quotes", h.CreateQuote, rateLimiter(20))
	api.POST("/chat", h.Chat, rateLimiter(20))

	// Public events
	api.GET("/events", h.ListPublicEvents)
	api.GET("/events/:slug", h.GetPublicEvent)
	api.POST("/events/:id/reserve", h.CreateReservation, rateLimiter(20))

	// Public products
	api.GET("/products", h.ListPublicProducts)
	api.GET("/products/images/:name", h.ServeProductImage)
	api.GET("/products/:slug", h.GetPublicProduct)

	// Invitation claim (public — unauthenticated student sets password)
	api.GET("/invitations/:token", h.PreviewInvitation)
	api.POST("/invitations/claim", h.ClaimInvitation, rateLimiter(20))

	// ── Protected (any authenticated user) ──────────────────────────────────
	protected := api.Group("")
	protected.Use(appmw.Auth(cfg))
	protected.GET("/auth/me", h.Me)
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
	admin.Use(appmw.RequireRoles(models.RoleSuperAdmin, models.RoleAdmin, models.RoleAdmissions))

	// Dashboard metrics
	admin.GET("/metrics", h.Metrics)

	// Quotes / contact
	admin.GET("/quotes", h.ListQuotes)
	admin.PATCH("/quotes/:id", h.AdminUpdateQuote)
	admin.POST("/quotes/:id/convert", h.AdminConvertQuote)

	// Staff directory (owner picker for the pipeline)
	admin.GET("/staff", h.AdminListStaff)

	// Enrollments
	admin.GET("/enrollments", h.ListEnrollments)
	admin.PATCH("/enrollments/:id", h.UpdateEnrollment)
	admin.POST("/enrollments/:id/invite", h.GenerateInvite)

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

	// Opportunities (B2B sales pipeline)
	admin.GET("/opportunities", h.AdminListOpportunities)
	admin.GET("/opportunities/forecast", h.AdminPipelineForecast)
	admin.GET("/accounts", h.AdminAccountsIndex)
	admin.POST("/opportunities", h.AdminCreateOpportunity)
	admin.PUT("/opportunities/:id", h.AdminUpdateOpportunity)
	admin.DELETE("/opportunities/:id", h.AdminDeleteOpportunity)

	// User management (super-admin + admin only)
	admin.POST("/users", h.CreateUser, appmw.RequireRoles(models.RoleSuperAdmin, models.RoleAdmin))

	listener, err := net.Listen("tcp4", "0.0.0.0:"+cfg.Port)
	if err != nil {
		log.Fatal(err)
	}
	e.Listener = listener
	log.Fatal(e.Start(""))
}
