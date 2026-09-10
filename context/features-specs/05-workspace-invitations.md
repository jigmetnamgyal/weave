Read `CLAUDE.md` before starting

We're adding workspace invitations (Unit M2.3): someone with
`member:invite` issues an invitation, the recipient accepts it, and
they join the workspace at the role they were invited to.

This is the unit that makes a workspace multiplayer. Until now a
workspace has exactly one member — its creator — and every collaborative
feature after this depends on a second person being able to get in.

## Decisions this unit locks in

- An invitation is a **capability**, not a notification. Holding the
  token is what grants entry, so it is treated like a credential:
  random, hashed at rest, single use, and expiring.
- An invitation carries a role, and issuing it obeys the same grant
  rule as changing a role (`Role.CanGrant` from M2.2). An admin cannot
  invite someone as an owner, because an admin cannot make an owner.
- **Weave does not send the email.** The inviter gets a link and shares
  it. Adding an email provider is a separate decision with its own
  deliverability, bounce and template concerns; inventing one here
  would smuggle a dependency into a membership unit.
- Record an ADR for the invitation token model — what is stored, why
  it is hashed, and what an attacker with database read access gains.

## Database

Add one migration creating `workspace_invitations`:

- `id` UUIDv7.
- `workspace_id` referencing `workspaces`, cascading on delete — an
  invitation to a workspace that no longer exists is meaningless.
- `email` citext — who it was issued to.
- `role` — constrained to the same four roles.
- `token_hash` — SHA-256 of the token, unique. **Never store the token
  itself.**
- `invited_by` referencing `users`.
- `expires_at`, `created_at` timestamptz.
- `accepted_at`, `accepted_by` — null until accepted.
- `revoked_at`, `revoked_by` — null unless revoked.

Rules the schema must hold:

- At most one *pending* invitation per (workspace, email). Use a
  partial unique index, so a revoked or accepted invitation does not
  block a later one — someone removed from a workspace must be
  invitable again.
- An invitation cannot be both accepted and revoked.
- Accepting sets `accepted_at` and `accepted_by` together.

## Domain

- `Invitation` in `internal/domain`, with a status derived from its
  timestamps rather than stored as a separate column that can disagree
  with them: pending, accepted, revoked, expired.
- Token generation: at least 256 bits from `crypto/rand`, encoded
  URL-safe. Hashing and comparison live here too.
- Default lifetime: 7 days. State it as a named constant.

## Authorization

- Issuing, listing and revoking require `member:invite` and the
  workspace-scoped membership check from M2.2 — so a non-member gets
  404 and a member without the permission gets 403.
- Issuing a role the inviter could not grant is refused with the same
  error as changing a role to it.
- **Accepting is different.** The acceptor is not yet a member, so it
  cannot sit behind the workspace membership check. It requires
  authentication only, and the token is the authorization.

## Accepting

- The signed-in user's email must match the invitation's, compared
  case-insensitively. A link that works for whoever opens it turns a
  forwarded email into a workspace breach.
- Accepting is atomic: create the membership, mark the invitation
  accepted, append the audit row — or none of it.
- Accepting twice is refused; the token is single use.
- An expired, revoked, already-accepted or unknown token all return
  **the same error**. Distinguishing them tells someone probing tokens
  which guesses were once real.
- If the user is already a member, refuse with a distinct, friendly
  error — that case is a mistake, not an attack, and they can already
  see the workspace.

## API

- `POST /v1/workspaces/{id}/invitations` — issue; returns the
  invitation **and the token, exactly once**. It is not retrievable
  afterwards.
- `GET /v1/workspaces/{id}/invitations` — list, filterable by status.
  Never returns tokens or hashes.
- `DELETE /v1/workspaces/{id}/invitations/{invitationId}` — revoke.
- `POST /v1/invitations/accept` — accept, with the token in the body,
  never in the URL. Query strings end up in logs, proxies and
  `Referer` headers.
- `GET /v1/invitations/preview` — given a token, return the workspace
  name and inviter so the acceptor knows what they are joining before
  committing. It must reveal nothing else, and must answer identically
  for an invalid token.
- Append an audit row for issue, revoke and accept, in the same
  transaction as the change.
- Update `contracts/openapi/openapi.yaml` alongside the handlers.

## Web

- A members page for the workspace, listing members and pending
  invitations.
- An invite form — email plus role — visible only to callers whose
  `permissions` include `member:invite`. The server still enforces it.
- After issuing, show the link once with a copy control, and say
  plainly that it will not be shown again.
- An accept route that previews the workspace, prompts sign-in when
  needed, and then accepts.
- Use the existing theme tokens and shadcn primitives. Do not edit
  `components/ui/*`.

## Tests

- Every status transition: pending → accepted, pending → revoked,
  pending → expired, and every refused transition out of a terminal
  state.
- Wrong email refused; matching email accepted case-insensitively.
- Expired, revoked, already-accepted and unknown tokens produce
  identical responses.
- The token is never returned by list or preview, and never appears in
  a log line.
- A stored `token_hash` cannot be used as a token.
- An admin cannot invite an owner; an owner can.
- Concurrent accepts of one token produce exactly one membership.
- Re-inviting someone previously removed succeeds.
- A second pending invitation for the same email is refused while one
  is outstanding, and allowed once it is revoked.
- Accepting adds the member at the invited role, not a default.

### Check when done

- An owner invites a second person, who accepts and appears in the
  member list at the invited role.
- The invited person sees the workspace in their own list, scoped
  correctly, with the permissions of their role.
- A viewer cannot see the invite form, and the API refuses them.
- A revoked link no longer works, and says the same thing as a link
  that never existed.
- A link opened by the wrong account is refused.
- `make ci` and `make test-integration` pass.
