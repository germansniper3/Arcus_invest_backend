// Package authz is the single source of truth for what a role may do.
//
// Design notes:
//   - Enforcement is default-deny. Permission is derived from the matched route
//     path, so a NEW admin route that is not mapped to a Resource is refused
//     rather than silently exposed. TestEveryAdminRouteIsMapped locks that in.
//   - Read access carries a Scope so a role can be limited to its own records
//     (ScopeOwn) rather than everything (ScopeAll). Notifications are the one
//     built-in use — an inbox is personal by definition; otherwise ScopeOwn is
//     the hook custom roles attach to.
//   - BuiltInGrants is the canonical grant set for built-in roles. Both
//     seed.Roles() and tests read from it — there is exactly one copy of the
//     truth, and the regression test runs without a database.
//   - At startup the grants are loaded from the database into an in-memory
//     cache (cachedRoles). If that fails the hardcoded matrix keeps working,
//     so a failed migration cannot lock everyone out.
package authz

import (
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"arcusinvest/internal/models"

	"gorm.io/gorm"
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
	ResRoles         Resource = "roles"
	ResGallery       Resource = "gallery"
	// Notifications are personal: a caller only ever sees their own, so this
	// grant is ScopeOwn and the handlers filter by user_id regardless.
	ResNotifications Resource = "notifications"
)

// AllResources is the enumeration used by the permissions payload and tests.
var AllResources = []Resource{
	ResOpportunities, ResAccounts, ResContracts, ResPayments, ResQuotes,
	ResEnrollments, ResStudents, ResEvents, ResProducts, ResUsers,
	ResAudit, ResEmail, ResMetrics, ResRoles, ResGallery, ResNotifications,
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

// ownInbox is read plus update (marking read) over the caller's own records
// only. Notifications are raised by the system, never authored or deleted by
// hand, so create and delete are deliberately absent.
func ownInbox() Grant {
	return Grant{
		Actions: map[Action]bool{ActionRead: true, ActionUpdate: true},
		Scope:   ScopeOwn,
	}
}

// BuiltInGrants is the canonical grant set for built-in roles. It is exported
// so seed.Roles() can write it to the database and tests can verify the seeded
// data matches the hardcoded truth exactly. Do NOT duplicate this data — read
// from here instead.
//
// super_admin and admin keep exactly the access they have today. admissions is
// deliberately narrowed to the student-intake domain: it previously reached the
// whole sales pipeline, contracts and payments purely because every staff role
// shared one route group.
var BuiltInGrants = map[models.Role]map[Resource]Grant{
	models.RoleSuperAdmin: {
		ResOpportunities: full(), ResAccounts: full(), ResContracts: full(),
		ResPayments: full(), ResQuotes: full(), ResEnrollments: full(),
		ResStudents: full(), ResEvents: full(), ResProducts: full(),
		ResUsers: full(), ResAudit: full(), ResEmail: full(), ResMetrics: readOnly(),
		ResRoles: full(), ResGallery: full(), ResNotifications: ownInbox(),
	},
	models.RoleAdmin: {
		ResOpportunities: full(), ResAccounts: full(), ResContracts: full(),
		ResPayments: full(), ResQuotes: full(), ResEnrollments: full(),
		ResStudents: full(), ResEvents: full(), ResProducts: full(),
		ResUsers: full(), ResAudit: full(), ResEmail: full(), ResMetrics: readOnly(),
		ResGallery: full(), ResNotifications: ownInbox(),
	},
	models.RoleAdmissions: {
		ResEnrollments:   full(),
		ResStudents:      full(),
		ResEvents:        full(),
		ResProducts:      readOnly(),
		ResMetrics:       readOnly(),
		ResNotifications: ownInbox(),
	},
	// Students never enter the admin group; their own routes are guarded
	// separately and scoped by user_id in the handlers.
	models.RoleStudent: {},
}

// matrix is the hardcoded fallback, initialised from BuiltInGrants. It is used
// when the database cache is not yet loaded or failed to load.
var matrix = func() map[models.Role]map[Resource]Grant {
	m := make(map[models.Role]map[Resource]Grant, len(BuiltInGrants))
	for role, grants := range BuiltInGrants {
		m[role] = grants
	}
	return m
}()

// ---------------------------------------------------------------------------
// In-memory cache — the DB-loaded grant map, swapped atomically.
// ---------------------------------------------------------------------------

var (
	cacheMu     sync.RWMutex
	cachedRoles map[models.Role]map[Resource]Grant // nil until LoadFromDB succeeds
)

// activeMatrix returns the effective grant map. If the DB-loaded cache is
// populated it wins; otherwise the hardcoded matrix fallback is used so a
// failed migration cannot lock everyone out.
//
// The returned map must NOT be mutated — callers treat it as read-only.
func activeMatrix() map[models.Role]map[Resource]Grant {
	cacheMu.RLock()
	m := cachedRoles
	cacheMu.RUnlock()
	if m != nil {
		return m
	}
	return matrix
}

// LoadFromDB reads all CustomRole + CustomRolePermission rows and builds a
// fresh grant map. The map is only published if it passes a sanity check
// (super_admin can read users), so a partial or corrupt load never locks
// everyone out.
func LoadFromDB(db *gorm.DB) error {
	var roles []models.CustomRole
	if err := db.Preload("Permissions").Find(&roles).Error; err != nil {
		return err
	}
	fresh := make(map[models.Role]map[Resource]Grant, len(roles))
	for _, role := range roles {
		grants := make(map[Resource]Grant, len(role.Permissions))
		for _, p := range role.Permissions {
			grants[Resource(p.Resource)] = Grant{
				Actions: map[Action]bool{
					ActionRead:   p.CanRead,
					ActionCreate: p.CanCreate,
					ActionUpdate: p.CanUpdate,
					ActionDelete: p.CanDelete,
				},
				Scope: Scope(p.Scope),
			}
		}
		fresh[models.Role(role.Name)] = grants
	}
	// Sanity check: super_admin must be able to read users. If not, the data
	// is corrupt or the seed hasn't run — refuse to publish.
	if !canInMatrix(fresh, models.RoleSuperAdmin, ResUsers, ActionRead) {
		log.Printf("authz: refusing to publish cache — super_admin cannot read users; using fallback")
		return nil
	}
	cacheMu.Lock()
	cachedRoles = fresh
	cacheMu.Unlock()
	return nil
}

// canInMatrix checks a specific permission in a given matrix without going
// through the global activeMatrix(). Used by the sanity check.
func canInMatrix(m map[models.Role]map[Resource]Grant, role models.Role, res Resource, act Action) bool {
	grant, ok := m[role][res]
	if !ok {
		return false
	}
	if act == ActionRead && grant.Scope == ScopeNone {
		return false
	}
	return grant.Actions[act]
}

// IsBuiltInRole reports whether a slug is one of the shipped roles in
// BuiltInGrants. Callers use this as a fallback when the custom_roles table is
// empty or unreadable, mirroring the hardcoded-matrix fallback so a failed role
// seed cannot make every role look invalid.
func IsBuiltInRole(name models.Role) bool {
	_, ok := BuiltInGrants[name]
	return ok
}

// RefreshCache reloads the grant map from the database. Errors are logged but
// do not crash the server — the existing cache (or hardcoded fallback) remains.
func RefreshCache(db *gorm.DB) {
	if err := LoadFromDB(db); err != nil {
		log.Printf("authz: cache refresh failed: %v", err)
	}
}

// StartCacheRefresher launches a background goroutine that re-reads the grant
// map from the database every interval. This ensures multi-replica coherence:
// a role edit on replica A reaches replica B within one interval.
//
// NOTE: This is the coherence mechanism. Sub-interval consistency is not
// guaranteed across replicas. Document this next to numReplicas in railway.json.
func StartCacheRefresher(db *gorm.DB, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			RefreshCache(db)
		}
	}()
}

// SetCacheForTest replaces the cached roles map. Test-only — production code
// must use LoadFromDB/RefreshCache. Caller must call with nil to restore the
// fallback after the test.
func SetCacheForTest(m map[models.Role]map[Resource]Grant) {
	cacheMu.Lock()
	cachedRoles = m
	cacheMu.Unlock()
}

// Can reports whether a role may perform an action on a resource.
func Can(role models.Role, res Resource, act Action) bool {
	grant, ok := activeMatrix()[role][res]
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
	grant, ok := activeMatrix()[role][res]
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
	"staff": ResOpportunities,
	// The aged receivables report is a view over money owed, so it follows
	// payment access rather than pipeline access — admissions has no business
	// reading the debtor book.
	"receivables":   ResPayments,
	"audit-logs":    ResAudit,
	"email":         ResEmail,
	"metrics":       ResMetrics,
	"roles":         ResRoles,
	"gallery":       ResGallery,
	"notifications": ResNotifications,
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
