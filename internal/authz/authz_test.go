package authz

import (
	"net/http"
	"testing"

	"arcusinvest/internal/models"
)

// TestBuiltInGrantsMatchHardcodedMatrix proves that BuiltInGrants produces
// byte-identical results to the hardcoded matrix across all roles, resources,
// and actions. This is the key regression test. It runs purely in-memory.
func TestBuiltInGrantsMatchHardcodedMatrix(t *testing.T) {
	for role, grants := range BuiltInGrants {
		for _, res := range AllResources {
			for _, act := range AllActions {
				want := false
				if g, ok := grants[res]; ok {
					if act != ActionRead || g.Scope != ScopeNone {
						want = g.Actions[act]
					}
				}
				got := Can(role, res, act)
				if got != want {
					t.Errorf("Can(%s, %s, %s) = %v, want %v", role, res, act, got, want)
				}
			}
			wantScope := ScopeNone
			if g, ok := grants[res]; ok && g.Actions[ActionRead] && g.Scope != "" {
				wantScope = g.Scope
			}
			gotScope := ReadScope(role, res)
			if gotScope != wantScope {
				t.Errorf("ReadScope(%s, %s) = %q, want %q", role, res, gotScope, wantScope)
			}
		}
	}
}

// TestCacheSwapCustomRole proves that a custom role set via SetCacheForTest
// works correctly for Can() and ReadScope(), including ScopeOwn.
func TestCacheSwapCustomRole(t *testing.T) {
	defer SetCacheForTest(nil) // restore fallback after test

	customRole := models.Role("sales_rep")
	testMap := map[models.Role]map[Resource]Grant{
		models.RoleSuperAdmin: BuiltInGrants[models.RoleSuperAdmin],
		customRole: {
			ResOpportunities: ownAll(),
			ResAccounts:      readOnly(),
		},
	}
	SetCacheForTest(testMap)

	if !Can(customRole, ResOpportunities, ActionRead) {
		t.Error("sales_rep should be able to read opportunities")
	}
	if !Can(customRole, ResOpportunities, ActionCreate) {
		t.Error("sales_rep should be able to create opportunities")
	}
	if Can(customRole, ResContracts, ActionRead) {
		t.Error("sales_rep should NOT be able to read contracts")
	}
	if scope := ReadScope(customRole, ResOpportunities); scope != ScopeOwn {
		t.Errorf("sales_rep opportunities scope = %q, want %q", scope, ScopeOwn)
	}
	if scope := ReadScope(customRole, ResAccounts); scope != ScopeAll {
		t.Errorf("sales_rep accounts scope = %q, want %q", scope, ScopeAll)
	}
}

// TestEmptyGrantRoleDenied proves that a role with zero grants in the cache is
// denied everything.
func TestEmptyGrantRoleDenied(t *testing.T) {
	defer SetCacheForTest(nil)

	emptyRole := models.Role("intern")
	testMap := map[models.Role]map[Resource]Grant{
		models.RoleSuperAdmin: BuiltInGrants[models.RoleSuperAdmin],
		emptyRole:             {},
	}
	SetCacheForTest(testMap)

	for _, res := range AllResources {
		for _, act := range AllActions {
			if Can(emptyRole, res, act) {
				t.Errorf("empty role must NOT %s %s", act, res)
			}
		}
		if s := ReadScope(emptyRole, res); s != ScopeNone {
			t.Errorf("empty role scope on %s = %q, want none", res, s)
		}
	}
}

// TestFallbackToHardcodedMatrix proves that when cachedRoles is nil, Can() and
// ReadScope() fall back to matrix (BuiltInGrants).
func TestFallbackToHardcodedMatrix(t *testing.T) {
	SetCacheForTest(nil)

	if !Can(models.RoleSuperAdmin, ResUsers, ActionRead) {
		t.Error("super_admin should read users using hardcoded matrix fallback")
	}
	if !Can(models.RoleSuperAdmin, ResRoles, ActionCreate) {
		t.Error("super_admin should create roles using hardcoded matrix fallback")
	}
	if Can(models.RoleAdmissions, ResUsers, ActionRead) {
		t.Error("admissions should NOT read users using hardcoded matrix fallback")
	}
}

// TestAdminsKeepFullAccess pins the requirement that this refactor does not
// reduce what super_admin and admin could already do.
func TestAdminsKeepFullAccess(t *testing.T) {
	for _, role := range []models.Role{models.RoleSuperAdmin, models.RoleAdmin} {
		for _, res := range AllResources {
			// Admin does not have roles access (only super_admin)
			if role == models.RoleAdmin && res == ResRoles {
				if Can(role, res, ActionRead) {
					t.Errorf("admin should NOT read roles")
				}
				continue
			}
			if !Can(role, res, ActionRead) {
				t.Errorf("%s should read %s", role, res)
			}
			if res == ResMetrics {
				continue // metrics is a read-only rollup
			}
			// An inbox is read and marked read, never authored or deleted by
			// hand — notifications are raised by the sweep. This is a new
			// resource, so no existing access is being reduced by excluding it.
			if res == ResNotifications {
				if !Can(role, res, ActionUpdate) {
					t.Errorf("%s should be able to mark notifications read", role)
				}
				for _, act := range []Action{ActionCreate, ActionDelete} {
					if Can(role, res, act) {
						t.Errorf("%s should NOT %s notifications — they are raised by the system", role, act)
					}
				}
				continue
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
		ResUsers, ResAudit, ResEmail, ResRoles,
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

// TestOnlySuperAdminCanAdministerRoles is the privilege-escalation guard.
//
// Authoring roles is equivalent to granting yourself any permission: whoever can
// write roles can create a role with full access and a user holding it (choosing
// that user's password), then log in as them. If any role other than super_admin
// gains write access to ResRoles, that escalation path reopens.
func TestOnlySuperAdminCanAdministerRoles(t *testing.T) {
	SetCacheForTest(nil)
	for role := range BuiltInGrants {
		if role == models.RoleSuperAdmin {
			continue
		}
		for _, act := range AllActions {
			if Can(role, ResRoles, act) {
				t.Errorf("PRIVILEGE ESCALATION: %s must NOT %s roles — only super_admin may administer roles", role, act)
			}
		}
	}
	// super_admin must retain it, or roles become unmanageable.
	for _, act := range AllActions {
		if !Can(models.RoleSuperAdmin, ResRoles, act) {
			t.Errorf("super_admin must be able to %s roles", act)
		}
	}
}

// TestRolesIsNotAliasedToUsers pins that role administration has its own resource.
// Mapping /admin/roles onto ResUsers would hand role authoring to every role with
// user management — i.e. admin — reopening the escalation path above.
func TestRolesIsNotAliasedToUsers(t *testing.T) {
	for _, p := range []string{"/api/v1/admin/roles", "/api/v1/admin/roles/:id"} {
		res, ok := ResourceForPath(p)
		if !ok {
			t.Fatalf("ResourceForPath(%q) not mapped", p)
		}
		if res != ResRoles {
			t.Errorf("ResourceForPath(%q) = %q, want %q — role admin must not be aliased to users", p, res, ResRoles)
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
		{"/api/v1/admin/roles", ResRoles, true},
		{"/api/v1/admin/roles/:id", ResRoles, true},
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
