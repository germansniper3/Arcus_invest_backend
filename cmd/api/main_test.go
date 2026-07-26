package main

import (
	"strings"
	"testing"

	"arcusinvest/internal/authz"
	"arcusinvest/internal/config"
	"arcusinvest/internal/handlers"

	"github.com/labstack/echo/v4"
)

// TestEveryAdminRouteIsPermissioned is the guard that makes the default-deny
// promise real. It walks the ACTUAL registered route table and fails if any
// admin route is not mapped to an authz resource.
//
// Without this, adding an admin endpoint and forgetting authz would make it
// reachable by every staff role. With it, the build fails instead — the fix is to
// add the path to authz.pathResources and grant it in the matrix.
//
// No database is touched: routes are registered against a nil handler, and the
// handlers are never invoked.
func TestEveryAdminRouteIsPermissioned(t *testing.T) {
	e := buildRouter(handlers.Handler{}, &config.Config{}, nil)

	var unmapped []string
	adminRoutes := 0
	for _, r := range e.Routes() {
		if !strings.HasPrefix(r.Path, "/api/v1/admin/") {
			continue
		}
		// Echo registers an internal catch-all for group 404s; it is not an
		// endpoint. Denying it is harmless and correct.
		if r.Method == echo.RouteNotFound {
			continue
		}
		adminRoutes++
		if _, ok := authz.ResourceForPath(r.Path); !ok {
			unmapped = append(unmapped, r.Method+" "+r.Path)
		}
	}

	if adminRoutes == 0 {
		t.Fatal("no admin routes discovered — buildRouter or the path prefix changed")
	}
	if len(unmapped) > 0 {
		t.Fatalf("%d admin route(s) are not mapped in authz and would be denied at runtime:\n  %s\n"+
			"Add the path segment to authz.pathResources and grant it in the matrix.",
			len(unmapped), strings.Join(unmapped, "\n  "))
	}
	t.Logf("%d admin routes, all permissioned", adminRoutes)
}

// TestEveryAdminRouteIsReachableBySuperAdmin catches the opposite failure: a
// route mapped to a resource that the top role cannot actually use, which would
// lock admins out of their own portal.
func TestEveryAdminRouteIsReachableBySuperAdmin(t *testing.T) {
	e := buildRouter(handlers.Handler{}, &config.Config{}, nil)

	for _, r := range e.Routes() {
		if !strings.HasPrefix(r.Path, "/api/v1/admin/") {
			continue
		}
		// Echo registers an internal catch-all for group 404s; it is not an
		// endpoint. Denying it is harmless and correct.
		if r.Method == echo.RouteNotFound {
			continue
		}
		res, ok := authz.ResourceForPath(r.Path)
		if !ok {
			continue // reported by TestEveryAdminRouteIsPermissioned
		}
		act := authz.ActionForMethod(r.Method)
		if !authz.Can("super_admin", res, act) {
			t.Errorf("super_admin cannot %s %s (%s %s) — this would lock admins out",
				act, res, r.Method, r.Path)
		}
	}
}
