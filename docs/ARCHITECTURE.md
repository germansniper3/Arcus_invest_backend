# Architecture

## Shape

Two independently deployed services against one Postgres database.

```
Browser ──► Frontend (React SPA, static)          arcusinvest.up.railway.app
   │
   └──────► Backend  (Go/Echo REST API)           backend-production-….up.railway.app
                │
                ├── PostgreSQL      (managed)
                ├── Object storage  (local disk in dev, S3-compatible in prod)
                ├── Resend          (outbound email over HTTPS)
                └── Anthropic API   (AI assistant, optional)
```

They are **separate origins**. That is the single fact that shapes the auth design: the refresh
cookie has to be `SameSite=None; Secure`, CORS has to allow credentials, and `CORS_ORIGINS` has to
list the frontend origin exactly, because browsers refuse credentialed requests against a wildcard.

The frontend is a static bundle. It holds no secrets — `VITE_API_BASE_URL` is the only thing baked
in, and every authority decision is made server-side.

---

## Request path

Every authenticated request passes through the same chain:

```
Auth ──► RejectStudents ──► RequirePermission ──► AuditMutations ──► handler
```

**`Auth`** validates the JWT signature, then re-reads the user row. The token proves *identity*
only; it is never trusted for *authority*. Role, active status and token version all come from the
database, so deactivating a user, changing their role, or revoking their sessions takes effect on
their very next request rather than lingering until the token expires.

**`RequirePermission`** is default-deny. It derives a resource from the matched route pattern and an
action from the HTTP verb, then asks the permission matrix. An admin route that nobody mapped is
refused — and a test walks the live route table to prove every admin route is mapped, so forgetting
is a build failure rather than a silent hole.

**`AuditMutations`** writes an `AuditLog` row after any successful mutating request, best-effort, so
the trail can never block or break the action it is recording.

Verb → action is fixed: `GET`/`HEAD` → read, `POST` → create, `PUT`/`PATCH` → update, `DELETE` →
delete. Anything else yields an empty action, which is always denied.

---

## Permissions

Authority is a matrix of **role × resource → {read, create, update, delete} + scope**.

There are 17 resources. Scope is `none`, `own` (only rows you own) or `all`. `own` is what lets a
sales rep see their own pipeline and nothing else; handlers apply it as a query filter.

Built-in roles (`super_admin`, `admin`, `admissions`, `student`) are defined in code as
`authz.BuiltInGrants` and re-seeded into the database on every boot, so code is the source of truth
and the database is a cache. Custom roles are created in-app and stored as rows.

A 30-second refresher reloads the cache. It is documented as eventually consistent across replicas —
which is exactly why session revocation was built on a column read per-request rather than an
in-process denylist.

The frontend receives the caller's effective permissions on `/auth/me` and uses them to hide what
they cannot use. That payload is **presentation only**. The `/admin` route gate asks the same
question — "can this user read anything?" — rather than checking a role list, because a hardcoded
list is a second source of truth and it drifted.

---

## Data model

37 models. The ones that carry most of the design:

**Identity** — `User`, `CustomRole`, `CustomRolePermission`, `RefreshToken`, `PasswordResetToken`.
`User.TokenVersion` is stamped into every access token and compared on every request.

**Hub** — `Enrollment`, `OnboardingInvitation`, `StudentProfile`, `CapstoneMilestone`,
`CapstoneComment`, `ProgressReport`, `ExtensionRequest`, `Submission`.

**CRM** — `QuoteRequest` (lead), `Opportunity` (deal) with `OpportunityContact`,
`OpportunityLineItem`, `OpportunityActivity`; `Payment`; `Contract`.

**Documents** — `DocumentVersion` and `DocumentAccessLog` are keyed by `(parent_type, parent_id)`,
so one pair of tables serves contracts and submissions rather than growing a table per attachment
point. `ContractSignature` and `UserSignature` carry signing evidence.

**Controls** — `ApprovalRule`, `ApprovalRequest`, `ApprovalDecision`, `Notification`,
`NotificationPreference`, `AuditLog`.

### Two conventions worth knowing

**Derived values are computed, never stored.** Weighted forecast, invoiced totals, receivable
ageing and balances are all calculated on read. Stored aggregates drift the moment anything is
edited out of band; computed ones cannot.

**Denormalised names sit beside foreign keys.** `ActorName`, `RequesterName`, `ApproverName`,
`UploadedBy` and similar are snapshots. A deleted user must not erase who approved a payment two
years ago.

Schema changes run through GORM `AutoMigrate` at boot. There are no migration files; the model
definitions are the schema.

---

## Frontend

`AdminPage.tsx` is one large component holding every admin section. Sections are local state, not
routes, so the admin area is a single URL.

That has one consequence worth naming: because sections are state, an expired session must not
unmount the tree. The re-authentication dialog renders *over* the app and never clears `user`,
which is what lets an open form survive an expiry — including a `File` handle and a signature
canvas, neither of which can be serialised to storage and restored.

The API client centralises three behaviours:

- **Cold-start retry** — network failures only, safe methods only, with backoff. An HTTP status is
  an answer and is never retried; a POST is never replayed on a guess, because a connection that
  drops mid-flight is indistinguishable from one refused outright.
- **Transparent refresh** — a 401 triggers a single shared refresh and one replay. Shared because
  rotation makes concurrent refreshes look like a token replay attack.
- **Typed errors** — `ApiError` carries status and body, so callers can distinguish "blocked
  pending approval" from "failed".

---

## Module map

```
backend/
  cmd/api/            route table, middleware chain, boot sequence
  internal/
    authz/            permission matrix, resources, path→resource mapping
    config/           environment loading and validation
    database/         connection + AutoMigrate
    handlers/         HTTP handlers, one file per domain
    middleware/       auth, permission, role gates
    models/           schema
    seed/             built-in roles, seed admin
    services/         auth, refresh, password reset, mail, notifications, PDF signing
    storage/          pluggable object storage

frontend/src/
  components/         Modal, NumberField, NotificationBell, SessionExpiredDialog, …
  lib/                api client, auth context, assets
  pages/              public site, login, reset, admin, student
  types/              shared API types
```
