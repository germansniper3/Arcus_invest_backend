// Package authz is the single source of truth for what a role may do.
//
// Design notes:
//   - Enforcement is default-deny. Permission is derived from the matched route
//     path, so a NEW admin route that is not mapped to a Resource is refused
//     rather than silently exposed. TestEveryAdminRouteIsMapped locks that in.
//   - Read access carries a Scope so a role can be limited to its own records
//     (ScopeOwn) rather than everything (ScopeAll). No built-in role uses
//     ScopeOwn today; it is the hook custom roles attach to.
package authz

import (
	"net/http"
	"strings"

	"arcusinvest/internal/models"
)

type Resource string

const (
	ResOpportunities Resource = "opportunities"
	ResAccounts      Resource = "accounts"
	ResContracts     Resource = "contracts"
	ResPayments      Resource = "payments"
	ResQuotes        Resource = "quotes"
	ResEnrollments   Resource = "enrollments"
	ResStudents      Resource = "students"
	ResEvents        Resource = "events"
	ResProducts      Resource = "products"
	ResUsers         Resource = "users"
	ResAudit         Resource = "audit"
	ResEmail         Resource = "email"
	ResMetrics       Resource = "metrics"
)

// AllResources is the enumeration used by the permissions payload and tests.
var AllResources = []Resource{
	ResOpportunities, ResAccounts, ResContracts, ResPayments, ResQuotes,
	ResEnrollments, ResStudents, ResEvents, ResProducts, ResUsers,
	ResAudit, ResEmail, ResMetrics,
}

type Action string

const (
	ActionRead   Action = "read"
	ActionCreate Action = "create"
	ActionUpdate Action = "update"
	ActionDelete Action = "delete"
)

var AllActions = []Action{ActionRead, ActionCreate, ActionUpdate, ActionDelete}

// Scope limits which rows a read may return.
type Scope string

const (
	ScopeNone Scope = "none" // no access
	ScopeOwn  Scope = "own"  // only rows the caller owns / is assigned
	ScopeAll  Scope = "all"  // every row
)

// Grant is one role's access to one resource.
type Grant struct {
	Actions map[Action]bool
	Scope   Scope
}

func full() Grant {
	return Grant{
		Actions: map[Action]bool{ActionRead: true, ActionCreate: true, ActionUpdate: true, ActionDelete: true},
		Scope:   ScopeAll,
	}
}

func readOnly() Grant {
	return Grant{Actions: map[Action]bool{ActionRead: true}, Scope: ScopeAll}
}

// ownAll gives every action but limits reads to the caller's own records. Unused
// by built-in roles; exercised by tests and available to custom roles.
func ownAll() Grant {
	return Grant{
		Actions: map[Action]bool{ActionRead: true, ActionCreate: true, ActionUpdate: true, ActionDelete: true},
		Scope:   ScopeOwn,
	}
}

// matrix maps role -> resource -> grant. A missing resource means no access.
//
// super_admin and admin keep exactly the access they have today. admissions is
// deliberately narrowed to the student-intake domain: it previously reached the
// whole sales pipeline, contracts and payments purely because every staff role
// shared one route group.
var matrix = map[models.Role]map[Resource]Grant{
	models.RoleSuperAdmin: {
		ResOpportunities: full(), ResAccounts: full(), ResContracts: full(),
		ResPayments: full(), ResQuotes: full(), ResEnrollments: full(),
		ResStudents: full(), ResEvents: full(), ResProducts: full(),
		ResUsers: full(), ResAudit: full(), ResEmail: full(), ResMetrics: readOnly(),
	},
	models.RoleAdmin: {
		ResOpportunities: full(), ResAccounts: full(), ResContracts: full(),
		ResPayments: full(), ResQuotes: full(), ResEnrollments: full(),
		ResStudents: full(), ResEvents: full(), ResProducts: full(),
		ResUsers: full(), ResAudit: full(), ResEmail: full(), ResMetrics: readOnly(),
	},
	models.RoleAdmissions: {
		ResEnrollments: full(),
		ResStudents:    full(),
		ResEvents:      full(),
		ResProducts:    readOnly(),
		ResMetrics:     readOnly(),
	},
	// Students never enter the admin group; their own routes are guarded
	// separately and scoped by user_id in the handlers.
	models.RoleStudent: {},
}

// Can reports whether a role may perform an action on a resource.
func Can(role models.Role, res Resource, act Action) bool {
	grant, ok := matrix[role][res]
	if !ok {
		return false
	}
	if act == ActionRead && grant.Scope == ScopeNone {
		return false
	}
	return grant.Actions[act]
}

// ReadScope reports how much of a resource a role may read.
func ReadScope(role models.Role, res Resource) Scope {
	grant, ok := matrix[role][res]
	if !ok || !grant.Actions[ActionRead] {
		return ScopeNone
	}
	if grant.Scope == "" {
		return ScopeNone
	}
	return grant.Scope
}

// PermissionsFor renders a role's effective permissions for the API, so the
// frontend can hide what the caller cannot use. The backend remains
// authoritative — this payload is presentation only.
func PermissionsFor(role models.Role) map[string]any {
	out := make(map[string]any, len(AllResources))
	for _, res := range AllResources {
		entry := map[string]any{"scope": string(ReadScope(role, res))}
		for _, act := range AllActions {
			entry[string(act)] = Can(role, res, act)
		}
		out[string(res)] = entry
	}
	return out
}

// ActionForMethod maps an HTTP verb to an action. Unknown verbs yield an empty
// action, which Can always denies.
func ActionForMethod(method string) Action {
	switch method {
	case http.MethodGet, http.MethodHead:
		return ActionRead
	case http.MethodPost:
		return ActionCreate
	case http.MethodPut, http.MethodPatch:
		return ActionUpdate
	case http.MethodDelete:
		return ActionDelete
	}
	return Action("")
}

// adminPrefix is the routing prefix every permissioned route sits under.
const adminPrefix = "/api/v1/admin/"

// pathResources maps the first path segment after the admin prefix to a
// resource. Sub-resources that deserve their own permission are handled by
// suffix in ResourceForPath.
var pathResources = map[string]Resource{
	"opportunities":    ResOpportunities,
	"accounts":         ResAccounts,
	"contracts":        ResContracts,
	"payments":         ResPayments,
	"quotes":           ResQuotes,
	"enrollments":      ResEnrollments,
	"students":         ResStudents,
	"progress-reports": ResStudents,
	"extensions":       ResStudents,
	"submissions":      ResStudents,
	"events":           ResEvents,
	"reservations":     ResEvents,
	"products":         ResProducts,
	"users":            ResUsers,
	// The staff directory exists to populate the pipeline's owner picker, so it
	// follows opportunity access rather than user administration.
	"staff":      ResOpportunities,
	"audit-logs": ResAudit,
	"email":      ResEmail,
	"metrics":    ResMetrics,
}

// ResourceForPath derives the resource a matched admin route acts on. The second
// return value is false when the path is not mapped, which callers MUST treat as
// deny — that is what makes a newly added, unmapped route fail closed.
func ResourceForPath(routePath string) (Resource, bool) {
	if !strings.HasPrefix(routePath, adminPrefix) {
		return "", false
	}
	rest := strings.TrimPrefix(routePath, adminPrefix)
	if rest == "" {
		return "", false
	}
	// Nested payment routes (…/opportunities/:id/payments) are payment access.
	if strings.HasSuffix(rest, "/payments") {
		return ResPayments, true
	}
	head := rest
	if i := strings.IndexByte(head, '/'); i >= 0 {
		head = head[:i]
	}
	res, ok := pathResources[head]
	return res, ok
}
