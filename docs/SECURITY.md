# Security

What the system enforces, and — equally important — what it does not claim to.

---

## Authority is never taken from the token

The JWT proves identity. Role, active status and token version are re-read from the database on
every request. Deactivating a user, changing their role, or revoking their sessions takes effect on
their next request rather than lingering until the token expires.

This costs one indexed primary-key lookup per request, and it is what makes everything below
possible without a second store.

---

## Sessions

| | |
|---|---|
| Access token | JWT, 30 minutes, `Authorization: Bearer` |
| Refresh token | 32 random bytes, 30 days, httpOnly cookie |
| Cookie | `HttpOnly; Secure; SameSite=None; Path=/api/v1/auth` |
| At rest | Only the SHA256 of the refresh token is stored |

**Why a cookie.** JavaScript cannot read it, so an XSS that reaches `localStorage` gets a token
worth at most 30 minutes instead of a 30-day session. `SameSite=None` is required because the
frontend and API are separate origins; browsers only honour it with `Secure`, which they permit on
`localhost`, so development behaves like production.

**Why hashed at rest.** A live refresh token *is* a session. A database dump must not contain
usable ones. (This deliberately differs from `OnboardingInvitation`, which stores its token in
plaintext — that grants one account setup, not a live session.)

### Rotation and replay detection

Every refresh consumes the presented token and issues a successor in the same *family*.

Rotation is what makes theft detectable: a thief and the real user cannot both keep using a chain,
so whichever presents a spent token reveals the theft. When that happens the **entire family is
revoked**, not just the replayed token — by then there is no way to tell which party is legitimate,
so both lose the session and someone re-authenticates.

Claiming a token is a conditional `UPDATE` checking rows-affected, so two requests arriving together
cannot both win and fork the chain. The loser receives a distinct 409 meaning "retry with the cookie
you now hold" rather than being treated as a replay — otherwise double-clicking would destroy a
legitimate session. The client cooperates by sharing one in-flight refresh across all callers.

### Revocation

`User.TokenVersion` is stamped into every access token and compared on each request. Incrementing it
strands every token already issued. It is a column rather than a denylist so revocation stays
coherent across replicas, and it is incremented with a SQL expression so concurrent revocations
cannot overwrite each other.

Each sign-in opens its own family, so signing out on one device leaves the others alone.
`logout-everywhere` revokes every family *and* bumps the version.

### Password reset

Single-use, hashed at rest, one-hour expiry, and requesting again supersedes the previous link.

Redeeming one **ends every session** — refresh families revoked and token version bumped, in one
transaction. Someone resetting because they believe their account is compromised has to be able to
evict whoever else is signed in; a password change that left sessions alive would not do that.

`forgot-password` returns an identical status and body whether or not the address exists, and sends
the email off the request path so timing does not differ either. Delivery failures are logged, not
returned — an error the caller can see is an answer the caller can use.

---

## Approval thresholds

Five actions can be gated on value: closing a deal as Won, signing a contract, recording a payment,
and deleting a deal or a contract.

Rules are `action + minimum amount + required approvals + approver role`. The highest applicable
floor wins, so one action can carry tiers. **No rule means not gated** — deploying the feature does
not freeze a working system.

An approval request records **intent**, not a stored mutation to replay. Approving does not perform
the action; it unblocks the requester, who retries and spends the approval at the gate. Signing
forces this design: the signer identity, IP and user agent in the evidence record must come from the
person actually signing, so an approver's click must never produce a signature.

Four properties, each with a test named for the failure it prevents:

| Property | Why |
|---|---|
| **No self-approval** | Checked on identity, not permission — a requester who holds the approver role is still refused. Without this the control is decorative. |
| **Single-use** | Consumption is a conditional `UPDATE`; two concurrent retries cannot both pass. |
| **Entity-bound** | An approval for contract A cannot delete contract B. |
| **Amount-bound** | An approval for K50,000 cannot admit a K500,000 action. |

`RequiredCount` means *N distinct people*, enforced by a composite unique index rather than a prior
`SELECT`. One rejection is decisive even when more approvals were required, and the reason is
mandatory — a rejection the requester cannot act on is a dead end, and the revise-and-resubmit loop
depends on it. Retrying after a rejection does not silently raise a fresh request; getting past a
"no" requires an explicit resubmit, linked to what it replaced.

Rules validation refuses anything unsatisfiable — an unknown action, a role that does not exist, or
a role that cannot decide approvals. Any of those would block an action permanently with nobody able
to release it, surfacing days later as "the app is broken" rather than as the settings typo it was.

Rules and the queue share one permission deliberately: anyone who can rewrite a threshold can
neutralise the control it configures, so rule administration must not be the softer of the two.

---

## Documents

Uploads **append a version** rather than overwrite. Losing a signed contract because someone
re-uploaded is a liability, not an inconvenience.

Each version stores a SHA256 computed in the same pass that writes the bytes — never recomputed
later from a file that could already have been replaced. That hash is what proves a document was not
swapped after the fact.

`AuditLog` covers mutations, which leaves "who downloaded this contract?" unanswerable — the one
question actually asked when a document leaks. `DocumentAccessLog` records reads separately, for
contracts and student submissions alike.

### Signing

PDF-only, stamped in pure Go. The unsigned original survives byte-for-byte as its own version. The
evidence record stores signer identity, timestamp, IP, user agent, and the hashes of **both** the
original and the stamped result, so it can be shown later that the thing signed is the thing on file.

`signed` is reachable **only** by signing — the contract update endpoint refuses to set it, so that
status always has an evidence record behind it.

**No legal claim is made.** The evidence is business-grade. Whether it constitutes a binding
signature under Zambian ECTA is a question for counsel, and nothing in the code or UI asserts that
it does. If counsel requires it, emailed one-time-code signer verification slots into the existing
record without changing what is already stored.

---

## Audit

Every successful mutating admin request writes an immutable `AuditLog` row: actor (id, name and role
snapshotted), action, entity, HTTP method, path and status. Approve, reject and resubmit are
recorded as distinct actions rather than a generic "update", so the two halves of a maker-checker
decision stay legible.

Approval requests and decisions are themselves permanent. Built-in roles are granted read, create
and update on approvals but **never delete** — an approval request is the evidence that an action was
authorised, and that must not be erasable by the people it constrains.

---

## Other controls

- **Passwords** — bcrypt, minimum length enforced by one shared validator rather than a check
  copied per call site.
- **Rate limiting** — per-IP on unauthenticated endpoints, each with its own budget. In-memory and
  therefore per-process, so it is a speed bump rather than the control; anti-enumeration rests on
  responses not varying.
- **Uploads** — 15 MB cap, extension whitelist, opaque server-generated keys, served only through
  authenticated routes and never a public static path.
- **Students** — rejected from the admin surface by a coarse gate before the permission check, and
  every student query is additionally scoped by user id. A foreign id returns 404, not 403.

---

## Known gaps

Stated plainly rather than left to be discovered.

| Gap | Detail |
|---|---|
| No 2FA | Recommended for privileged accounts; not built. |
| Rate limiter is per-process | Weakens with a second replica. |
| Notification worker is per-process | Every replica would run the sweep and send duplicate email. Fine at one replica, which is the current configuration. |
| Reset email delivery unverified | The token lifecycle, revocation and UI are verified end to end; the actual delivery hop has not been exercised. |
| Document retention undecided | Soft-deleting a contract leaves its versions, signatures and stored files behind. For signature evidence that may be correct — but it should be a decision, not an accident. |
