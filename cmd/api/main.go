package main

import (
	"arcusinvest/internal/config"
	"arcusinvest/internal/database"
	"arcusinvest/internal/handlers"
	appmw "arcusinvest/internal/middleware"
	"arcusinvest/internal/models"
	"arcusinvest/internal/seed"
	"log"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

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
	if err := seed.Products(db); err != nil {
		log.Fatal(err)
	}

	e := echo.New()
	e.HideBanner = true
	e.Use(middleware.Recover())
	e.Use(middleware.Logger())
	e.Use(middleware.CORSWithConfig(middleware.CORSConfig{
		AllowOrigins: cfg.CORSOrigins,
		AllowMethods: []string{echo.GET, echo.POST, echo.PUT, echo.PATCH, echo.DELETE, echo.OPTIONS},
		AllowHeaders: []string{echo.HeaderOrigin, echo.HeaderContentType, echo.HeaderAccept, echo.HeaderAuthorization},
	}))

	h := handlers.Handler{DB: db, Cfg: cfg}
	api := e.Group("/api/v1")

	// ── Public ──────────────────────────────────────────────────────────────
	api.GET("/health", h.Health)
	api.POST("/auth/login", h.Login)
	api.POST("/enrollments", h.CreateEnrollment)
	api.POST("/quotes", h.CreateQuote)
	api.POST("/chat", h.Chat)

	// Public events
	api.GET("/events", h.ListPublicEvents)
	api.GET("/events/:slug", h.GetPublicEvent)
	api.POST("/events/:id/reserve", h.CreateReservation)

	// Public products
	api.GET("/products", h.ListPublicProducts)
	api.GET("/products/:slug", h.GetPublicProduct)

	// Invitation claim (public — unauthenticated student sets password)
	api.GET("/invitations/:token", h.PreviewInvitation)
	api.POST("/invitations/claim", h.ClaimInvitation)

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

	// ── Admin ────────────────────────────────────────────────────────────────
	admin := protected.Group("/admin")
	admin.Use(appmw.RequireRoles(models.RoleSuperAdmin, models.RoleAdmin, models.RoleAdmissions))

	// Dashboard metrics
	admin.GET("/metrics", h.Metrics)

	// Quotes / contact
	admin.GET("/quotes", h.ListQuotes)
	admin.PATCH("/quotes/:id", h.AdminUpdateQuote)

	// Enrollments
	admin.GET("/enrollments", h.ListEnrollments)
	admin.PATCH("/enrollments/:id", h.UpdateEnrollment)
	admin.POST("/enrollments/:id/invite", h.GenerateInvite)

	// Students (hub portal — admin-only visibility)
	admin.GET("/students", h.ListStudents)
	admin.GET("/students/:id", h.AdminStudentDetail)
	admin.POST("/students/:id/comments", h.AdminPostComment)
	admin.PATCH("/students/:id/milestones/:mid", h.AdminUpdateMilestone)

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
	admin.PUT("/products/:id", h.AdminUpdateProduct)
	admin.DELETE("/products/:id", h.AdminDeleteProduct)

	// User management (super-admin + admin only)
	admin.POST("/users", h.CreateUser, appmw.RequireRoles(models.RoleSuperAdmin, models.RoleAdmin))

	log.Fatal(e.Start(":" + cfg.Port))
}
