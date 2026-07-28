# API Reference

Base path: `/api/v1`. JSON in, JSON out. Errors are `{"error": "..."}`.

Four surfaces:

| Surface | Auth | Gate |
|---|---|---|
| **Public** | none | per-IP rate limits |
| **Auth** | none or cookie | rate limits |
| **Student** | bearer token | `student` role, every query scoped to the caller |
| **Admin** | bearer token | default-deny permission matrix |

Admin permissions are shown as `resource:action`, derived from the route's first path segment and
the HTTP verb (`GET`→read, `POST`→create, `PUT`/`PATCH`→update, `DELETE`→delete). An admin route not
present in the mapping is refused, and a test walks the live route table to prove none is missing.

---

## Auth

| Method | Path | Notes |
|---|---|---|
| POST | `/auth/login` | Returns `{token, user}` and sets the refresh cookie. |
| POST | `/auth/refresh` | Cookie-authenticated. Rotates the cookie, returns a new access token. `409` means a concurrent refresh — retry. |
| POST | `/auth/logout` | Revokes this session's refresh family and clears the cookie. Always succeeds. |
| POST | `/auth/forgot-password` | **Always 200**, whether or not the address exists. |
| POST | `/auth/reset-password` | Redeems a link, sets the password, ends every session. |
| GET | `/auth/me` | Bearer. Returns the user plus their effective permissions. |
| POST | `/auth/logout-everywhere` | Bearer. Revokes every family and bumps the token version. |

Refresh, logout and reset must be called with `credentials: 'include'`, or the cookie is dropped
silently.

---

## Public

| Method | Path | Notes |
|---|---|---|
| GET | `/health` | Liveness. Used by the platform healthcheck. |
| POST | `/enrollments` | Innovation Hub application. Admin-only fields are stripped. |
| POST | `/quotes` | Lead capture from the marketing site. |
| POST | `/chat` | AI assistant; falls back to a local knowledge base with no API key. |
| GET | `/events` · `/events/:slug` | Published events only. |
| POST | `/events/:id/reserve` | Public reservation request. |
| GET | `/products` · `/products/:slug` · `/products/images/:name` | Published catalogue. |
| GET | `/gallery` · `/gallery/images/:name` | Published gallery. |
| GET | `/invitations/:token` | Preview an invitation. `410` when expired. |
| POST | `/invitations/claim` | Sets the password and provisions the account. |

---

## Student

All under `/student`, scoped to the authenticated student. A foreign id returns `404`, not `403`.

| Method | Path | Notes |
|---|---|---|
| GET | `/dashboard` | Profile, milestones, comments, reports, extensions, submissions. |
| PATCH | `/capstone` | Title and summary. |
| PATCH | `/milestones/:mid` | Advance own milestone. **Cannot** set `completed` or write feedback. |
| POST | `/comments` | Post to the shared mentor feed. |
| POST | `/progress-reports` · `/extensions` · `/submissions` | Submit for review. |
| GET | `/submissions/:id/file` | Download own submission; logged to `DocumentAccessLog`. |

---

## Admin

### Pipeline — `opportunities`

| Method | Path | Permission |
|---|---|---|
| GET | `/opportunities` · `/opportunities/forecast` · `/opportunities/export` | `opportunities:read` |
| POST | `/opportunities` | `opportunities:create` |
| PUT | `/opportunities/:id` | `opportunities:update` — **gated** on close-won |
| DELETE | `/opportunities/:id` | `opportunities:delete` — **gated** |
| PATCH | `/opportunities/:id/invoiced` | `opportunities:update` |
| GET/POST | `/opportunities/:id/activities` | `opportunities:read` / `:create` |
| GET | `/staff` | `opportunities:read` — owner picker |

### Accounts — `accounts`

`GET /accounts`, `/accounts/:name/payments`, `/accounts/:name/recommendations`

### Money — `payments`

| Method | Path | Permission |
|---|---|---|
| GET/POST | `/opportunities/:id/payments` | `payments:read` / `:create` — **gated** on create |
| DELETE | `/payments/:id` | `payments:delete` |
| GET | `/receivables` · `/receivables/export` · `/payments/export` | `payments:read` |

Receivables are computed live from line items and recorded payments. Nothing is stored, so it
cannot drift.

### Contracts — `contracts`

| Method | Path | Permission |
|---|---|---|
| GET/POST | `/contracts` | `contracts:read` / `:create` |
| PUT | `/contracts/:id` | `contracts:update` — cannot set `signed` |
| DELETE | `/contracts/:id` | `contracts:delete` — **gated** |
| POST | `/contracts/:id/file` | `contracts:create` — appends a version |
| GET | `/contracts/:id/file` · `/versions` · `/versions/:versionId/file` | `contracts:read` — reads logged |
| GET | `/contracts/:id/access-log` · `/signatures` | `contracts:read` |
| POST | `/contracts/:id/sign` | `contracts:create` — **gated**; PDF only |
| GET/DELETE | `/contracts/my-signature` | the caller's own saved signature |

### Approvals — `approvals`

| Method | Path | Notes |
|---|---|---|
| GET | `/approvals` | Filters: `status`, `mine`, `awaiting` |
| GET | `/approvals/:id` | With decisions |
| PATCH | `/approvals/:id/approve` · `/reject` | `reject` requires a reason. Audited as distinct actions. |
| POST | `/approvals/:id/resubmit` | Original requester only; links via `supersedes_id` |
| GET/POST | `/approval-rules` | |
| PATCH | `/approval-rules/:id` | Validation refuses unsatisfiable rules |

`PATCH` not `POST` for decisions: a `POST` would require a create grant approvers must not have.

### Notifications — `notifications`

`GET /notifications`, `PATCH /notifications/:id/read`, `PATCH /notifications/read-all`,
`GET`/`PUT /notifications/preferences`. Read + update only, scoped to the caller — nobody authors or
deletes their own inbox.

### Intake and students — `enrollments`, `students`

`GET`/`POST /enrollments`, `PATCH /enrollments/:id`, `POST /enrollments/:id/invite`;
`GET /students`, `GET /students/:id`, `PATCH /students/:id/milestones/:mid`,
`POST /students/:id/comments`, `PATCH /progress-reports/:id`, `PATCH /extensions/:id`,
`PATCH /submissions/:id`, `GET /submissions/:id/file`.

Progress reports, extensions and submissions all map to `students`.

### Catalogue and events — `products`, `events`, `gallery`

`products` and `gallery` are full CRUD plus image upload. `events` covers events, reservations
(`PATCH /reservations/:rid/approve`) and `POST /events/:id/broadcast`.

### Administration — `users`, `roles`, `audit`, `email`, `metrics`

`GET`/`POST /users`, `PATCH`/`DELETE /users/:id`; `roles` CRUD (super-admin only);
`GET /audit-logs`; `GET /email/status`, `POST /email/test`; `GET /metrics`.

---

## Status codes with specific meaning

| Code | Meaning |
|---|---|
| `401` | Not authenticated, token expired, or **session revoked**. The client refreshes and replays once before surfacing it. |
| `403` | Authenticated but not permitted. Also self-approval. |
| `409` on a mutation | **Blocked pending approval.** Body carries `approval_request_id` and `approval_status`. Not a failure — the action may proceed once someone signs off. |
| `409` on `/auth/refresh` | Concurrent refresh; retry with the cookie you now hold. |
| `410` | Invitation expired. |

`409` rather than `403` for a gated action is deliberate: `403` says "you may not", which is the
wrong claim — the caller may, once someone else agrees.
