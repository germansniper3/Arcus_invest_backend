package authz

import (
	"net/http"
	"testing"

	"arcusinvest/internal/models"
)

// TestAdminsKeepFullAccess pins the requirement that this refactor does not
// reduce what super_admin and admin could already do.
func TestAdminsKeepFullAccess(t *testing.T) {
	for _, role := range []models.Role{models.RoleSuperAdmin, models.RoleAdmin} {
		for _, res := range AllResources {
			if !Can(role, res, ActionRead) {
				t.Errorf("%s should read %s", role, res)
			}
			if res == ResMetrics {
				continue // metrics is a read-only rollup
			}
			for _, act := range []Action{ActionCreate, ActionUpdate, ActionDelete} {
				if !Can(role, res, act) {
					t.Errorf("%s should %s %s", role, act, res)
				}
			}
		}
	}
}

// TestAdmissionsIsConfinedToIntake is the security requirement behind the change:
// admissions previously reached the entire sales surface because all staff roles
// shared one route group.
func TestAdmissionsIsConfinedToIntake(t *testing.T) {
	denied := []Resource{
		ResOpportunities, ResAccounts, ResContracts, ResPayments, ResQuotes,
		ResUsers, ResAudit, ResEmail,
	}
	for _, res := range denied {
		for _, act := range AllActions {
			if Can(models.RoleAdmissions, res, act) {
				t.Errorf("admissions must NOT %s %s", act, res)
			}
		}
		if s := ReadScope(models.RoleAdmissions, res); s != ScopeNone {
			t.Errorf("admissions read scope on %s = %q, want none", res, s)
		}
	}

	allowed := []Resource{ResEnrollments, ResStudents, ResEvents}
	for _, res := range allowed {
		for _, act := range AllActions {
			if !Can(models.RoleAdmissions, res, act) {
				t.Errorf("admissions should %s %s", act, res)
			}
		}
	}
	// Read-only resources: readable, never writable.
	for _, res := range []Resource{ResProducts, ResMetrics} {
		if !Can(models.RoleAdmissions, res, ActionRead) {
			t.Errorf("admissions should read %s", res)
		}
		for _, act := range []Action{ActionCreate, ActionUpdate, ActionDelete} {
			if Can(models.RoleAdmissions, res, act) {
				t.Errorf("admissions must NOT %s %s", act, res)
			}
		}
	}
}

// TestStudentsHaveNoAdminAccess — students are guarded by their own group, but
// the matrix must deny them regardless.
func TestStudentsHaveNoAdminAccess(t *testing.T) {
	for _, res := range AllResources {
		for _, act := range AllActions {
			if Can(models.RoleStudent, res, act) {
				t.Errorf("student must NOT %s %s", act, res)
			}
		}
	}
}

// TestUnknownRoleIsDenied — an unrecognised role (stale token, future custom
// role not yet in the matrix) must fail closed.
func TestUnknownRoleIsDenied(t *testing.T) {
	for _, res := range AllResources {
		for _, act := range AllActions {
			if Can(models.Role("sales_rep"), res, act) {
				t.Errorf("unknown role must NOT %s %s", act, res)
			}
		}
		if s := ReadScope(models.Role(""), res); s != ScopeNone {
			t.Errorf("empty role scope on %s = %q, want none", res, s)
		}
	}
}

func TestReadScopeValues(t *testing.T) {
	if got := ReadScope(models.RoleAdmin, ResOpportunities); got != ScopeAll {
		t.Errorf("admin opportunities scope = %q, want all", got)
	}
	if got := ReadScope(models.RoleAdmissions, ResEnrollments); got != ScopeAll {
		t.Errorf("admissions enrollments scope = %q, want all", got)
	}
	if got := ReadScope(models.RoleAdmissions, ResContracts); got != ScopeNone {
		t.Errorf("admissions contracts scope = %q, want none", got)
	}
}

// TestOwnScopeGrantShape guards the ScopeOwn hook that custom roles will attach
// to, so it cannot silently rot while unused by built-in roles.
func TestOwnScopeGrantShape(t *testing.T) {
	g := ownAll()
	if g.Scope != ScopeOwn {
		t.Fatalf("ownAll scope = %q, want own", g.Scope)
	}
	for _, act := range AllActions {
		if !g.Actions[act] {
			t.Errorf("ownAll should allow %s", act)
		}
	}
}

func TestActionForMethod(t *testing.T) {
	cases := map[string]Action{
		http.MethodGet:    ActionRead,
		http.MethodHead:   ActionRead,
		http.MethodPost:   ActionCreate,
		http.MethodPut:    ActionUpdate,
		http.MethodPatch:  ActionUpdate,
		http.MethodDelete: ActionDelete,
		"TRACE":           Action(""),
	}
	for method, want := range cases {
		if got := ActionForMethod(method); got != want {
			t.Errorf("ActionForMethod(%s) = %q, want %q", method, got, want)
		}
	}
	// An unmapped verb must be denied for every role/resource.
	if Can(models.RoleSuperAdmin, ResUsers, ActionForMethod("TRACE")) {
		t.Error("unmapped HTTP verb must be denied even for super_admin")
	}
}

func TestResourceForPath(t *testing.T) {
	cases := []struct {
		path string
		want Resource
		ok   bool
	}{
		{"/api/v1/admin/opportunities", ResOpportunities, true},
		{"/api/v1/admin/opportunities/:id", ResOpportunities, true},
		{"/api/v1/admin/opportunities/:id/activities", ResOpportunities, true},
		{"/api/v1/admin/opportunities/forecast", ResOpportunities, true},
		{"/api/v1/admin/opportunities/:id/payments", ResPayments, true},
		{"/api/v1/admin/payments/:id", ResPayments, true},
		{"/api/v1/admin/accounts/:name/recommendations", ResAccounts, true},
		{"/api/v1/admin/contracts/:id/file", ResContracts, true},
		{"/api/v1/admin/progress-reports/:id", ResStudents, true},
		{"/api/v1/admin/reservations/:rid/approve", ResEvents, true},
		{"/api/v1/admin/staff", ResOpportunities, true},
		{"/api/v1/admin/audit-logs", ResAudit, true},
		{"/api/v1/admin/email/test", ResEmail, true},
		{"/api/v1/admin/users/:id", ResUsers, true},
		// Not permissioned / unmapped -> must fail closed.
		{"/api/v1/admin/", "", false},
		{"/api/v1/admin/brand-new-thing", "", false},
		{"/api/v1/auth/me", "", false},
		{"/api/v1/student/dashboard", "", false},
	}
	for _, tc := range cases {
		got, ok := ResourceForPath(tc.path)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ResourceForPath(%q) = (%q,%v), want (%q,%v)", tc.path, got, ok, tc.want, tc.ok)
		}
	}
}

func TestPermissionsForShape(t *testing.T) {
	perms := PermissionsFor(models.RoleAdmissions)
	if len(perms) != len(AllResources) {
		t.Fatalf("permissions has %d resources, want %d", len(perms), len(AllResources))
	}
	contracts, ok := perms[string(ResContracts)].(map[string]any)
	if !ok {
		t.Fatal("contracts entry missing")
	}
	if contracts["read"] != false {
		t.Error("admissions contracts.read should be false")
	}
	if contracts["scope"] != string(ScopeNone) {
		t.Errorf("admissions contracts.scope = %v, want none", contracts["scope"])
	}
}
