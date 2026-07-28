# Operations

## Deployment

Two Railway services plus managed Postgres, in one project.

| Service | Repo | URL |
|---|---|---|
| `frontend` | `Arcus_invest_frontend` | `arcusinvest.up.railway.app` |
| `backend` | `Arcus_invest_backend` | `backend-production-….up.railway.app` |
| `Postgres` | — | private network |

Both deploy automatically from `main`. The backend is **configured to wait for CI**: if the GitHub
Actions run fails, Railway marks the deployment `SKIPPED` and production keeps running the previous
build. A `SKIPPED` deployment therefore means "CI is red", not "something went wrong with Railway" —
check the workflow run before doing anything else.

The backend builds from its `Dockerfile` and is healthchecked at `/api/v1/health`.

> **The repositories are separate.** `backend/` and `frontend/` are independent git repos, each with
> its own remotes. There is no monorepo root repo. Changes spanning both require two commits and two
> pushes.

### Railway CLI

The `backend/` working directory is linked to the **frontend** service. Always pass `--service`
explicitly:

```bash
railway status --service backend
railway variables --service backend
railway deployment list --service backend
railway logs --service backend
```

Running these from `frontend/` fails — that directory is not linked to the project.

---

## Environment

### Required in production

| Variable | Notes |
|---|---|
| `DATABASE_URL` | Managed Postgres, SSL on |
| `JWT_SECRET` | Long and random. Changing it signs everyone out. |
| `FRONTEND_URL` | Builds invitation and password-reset links |
| `CORS_ORIGINS` | **Must list the frontend origin exactly.** The refresh cookie is sent with credentials, and browsers refuse credentialed requests against a wildcard — a missing origin breaks sign-in outright rather than degrading. |
| `SEED_ADMIN_PASSWORD` | The server refuses to start without it when `ENV=production` |

### Sessions

| Variable | Default |
|---|---|
| `ACCESS_TOKEN_TTL_MINUTES` | 30 |
| `REFRESH_TOKEN_TTL_DAYS` | 30 |

`JWT_TTL_HOURS` is superseded. It is not silently ignored — if still set, the server logs a warning
at boot naming what overrode it.

### Email

`MAIL_FROM` plus either `RESEND_API_KEY` (HTTPS) or the `SMTP_*` set. Resend wins when both are
present.

**Railway blocks outbound SMTP below the Pro plan** — ports 25/465/587 are closed and present as a
connection timeout regardless of settings. Use Resend. Verify the sending domain at
resend.com/domains, or delivery is limited to your own account address.

With neither configured, invitation links are still generated for manual sharing and broadcasts stay
`queued`. Verify from **Admin → Users → Send test email**, which delivers only to the caller.

### Other

`STORAGE_DRIVER` (`local` or `s3`) with `STORAGE_DIR` or the `S3_*` set; `AI_API_KEY`,
`AI_PROVIDER_URL`, `AI_MODEL` (with no key the assistant answers from a local knowledge base rather
than failing in front of a visitor).

---

## Schema changes

GORM `AutoMigrate` runs at boot and is **fatal on failure**, so a healthy service is proof the
migration applied.

Adding a `NOT NULL` column with a default backfills existing rows — worth verifying explicitly when
the column gates access, as `users.token_version` does. A zero there would have locked every account
out rather than merely asking for a fresh sign-in.

There is no down-migration path. Destructive schema changes need a plan before they are written.

---

## CI

`.github/workflows/ci.yml` runs on every push to `main` and on pull requests: a throwaway Postgres
container, then `go vet`, `go build`, `go test ./... -count=1`.

DB-backed tests run inside a transaction that is rolled back, so CI's database — and yours — is left
as it was found.

**CI starts from an empty database.** Tests must seed anything they depend on. A test that leans on
data your development database happens to hold will pass locally and fail in CI; that has already
happened once, with roles seeded only by `main()`.

---

## Runbook

### A deploy did not appear

Check `railway deployment list --service backend`. `SKIPPED` means CI failed — fix the workflow run
and push again rather than forcing a deploy. `BUILDING` for a long time means look at build logs.

### Everyone was signed out

Expected after any change to `JWT_SECRET`, or after a deploy that changes token claim structure.
Users sign in again; nothing is lost.

### Sign-in broken after a domain change

Almost certainly `CORS_ORIGINS`. With credentials enabled the origin must match exactly — scheme,
host and port. Update it on the backend service and redeploy.

### Verifying production without credentials

`POST /auth/forgot-password` always returns `200` and reveals nothing, so it is a safe liveness probe
for the auth stack. Use an address that does not exist — a real one sends a real reset email to a
real person.

### The container sleeps

Railway idles the service. The first request after a sleep is refused; the client retries with
backoff and shows a waking state rather than signing the user out. A cold start is not an incident.
