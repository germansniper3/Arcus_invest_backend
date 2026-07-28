# Arcus Investments Platform

Two products in one codebase, sharing a single identity and permission system:

- **Arcus Innovation Hub** — an admissions and training pipeline. Applications, invitations,
  capstone milestones, progress reports, extension requests and file submissions.
- **Commercial CRM** — the B2B side. Leads → deals → quotations → contracts → payments, with
  document versioning, contract signing, an aged debtor book, and value-based approval controls.

Plus a public marketing site, a product catalogue, an events manager, and an AI assistant.

**Live:** [arcusinvest.up.railway.app](https://arcusinvest.up.railway.app)

---

## Stack

| | |
|---|---|
| Frontend | React 19, TypeScript, Vite, React Router, Radix primitives, Framer Motion |
| Backend | Go, Echo, GORM, PostgreSQL |
| Auth | JWT access tokens + rotating refresh tokens in an httpOnly cookie |
| Storage | Pluggable — local disk in development, S3-compatible in production |
| Email | Resend HTTPS API, or SMTP where outbound ports are open |
| Hosting | Railway (two services + managed Postgres), deploying from `main` on CI green |

No Tailwind and no component kit. Styling is plain CSS in `frontend/src/styles.css` plus inline
styles; Radix supplies unstyled behaviour (dialogs) only.

---

## Documentation

| Document | What it covers |
|---|---|
| [Architecture](docs/ARCHITECTURE.md) | Request path, data model, permission system, module map |
| [Security](docs/SECURITY.md) | Sessions, approvals, document integrity, audit trail, known gaps |
| [API](docs/API.md) | Every route, grouped by surface, with the permission each requires |
| [Operations](docs/OPERATIONS.md) | Environment variables, deployment, CI, runbook |

> **This repo is the backend.** The frontend is a separate repository
> (`Arcus_invest_frontend`), checked out alongside this one as `../frontend`. There is no monorepo
> root — a change spanning both needs two commits and two pushes.

---

## Local setup

Requires Docker, Go 1.25+, and Node 20+. The instructions below assume both repos are checked out
side by side as `backend/` and `frontend/`.

```bash
docker compose up -d postgres
```

Postgres is exposed on host port **5434** to avoid colliding with a native install on 5432.

```bash
cd backend && cp .env.example .env && go run ./cmd/api
```

```bash
cd frontend && cp .env.example .env && npm install && npm run dev -- --port 5179
```

The API listens on `:8032`; the frontend on `:5179`. If you change the frontend port, add it to
`CORS_ORIGINS` in `backend/.env` — the refresh cookie is sent with credentials, and browsers refuse
credentialed requests against an origin that is not explicitly allowed.

Then open:

- Public site — `http://localhost:5179`
- Enrollment form — `http://localhost:5179/arcus-innovation-hub-enrollment-manager`
- Staff sign-in — `http://localhost:5179/login` (deliberately unlinked from the public nav)

### Tests

```bash
cd backend && go build ./... && go vet ./... && go test ./... -count=1
```

DB-backed tests skip when `DATABASE_URL` is unset and otherwise run inside a transaction that is
rolled back, so a run leaves the database exactly as it found it.

```bash
cd frontend && npx tsc --noEmit && npm run build
```

---

## Seeding

The server seeds exactly one super-admin at boot from `SEED_ADMIN_NAME` / `SEED_ADMIN_EMAIL` /
`SEED_ADMIN_PASSWORD`, and refuses to start in production without an explicit password. Built-in
roles are seeded from `authz.BuiltInGrants` on every boot, so the permission matrix in code is
always the one in the database.

No demo data. Products, events, staff and deals are all created in-app.

---

## The two pipelines

### Innovation Hub

`apply → review → invite → claim → track`

A public application captures tier, interests and project idea, with admin-only fields stripped
server-side. Staff move it through `submitted → pending_orientation → orientation_complete →
accepted`, then issue a single-use invitation. Claiming provisions the account, profile, a six-step
capstone plan and a welcome message in one transaction.

Students advance their own milestones to `pending_review`; only a mentor can sign off `completed`.
Students can never write the feedback field, never reach another student's rows, and never set a
review outcome — all enforced at the API, not the UI.

### Commercial CRM

`lead → deal → quotation → contract → payment`

Deals carry a pipeline stage, a grade, a buying committee, line items and an activity log.
Quotations and invoices are generated from line items. Contracts store versioned documents with
SHA256 hashes and can be signed in-browser with a stamped PDF and an evidence record. Payments are
recorded against deals and feed an aged receivables report computed live from line items and
payments — never stored, so it cannot drift.

High-consequence actions are gated on value. See [Security](docs/SECURITY.md#approval-thresholds).

---

## What this system deliberately does not do

- **It does not move money.** Payments are *recorded*, not processed. There is no payment gateway
  and no funds transfer anywhere in the codebase.
- **It makes no legal claim about signatures.** Contract signing captures business-grade evidence
  (signer identity, timestamp, IP, user agent, hashes of both the original and stamped documents).
  Whether that constitutes a binding signature under Zambian ECTA is a question for counsel, and
  no wording in the product asserts that it does.
