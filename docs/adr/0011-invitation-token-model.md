# ADR-011: Invitations are bearer capabilities, stored only as hashes

- Status: Accepted
- Date: 2026-09-11
- Related: ADR-009 (Clerk for authentication only), ADR-010 (tenancy)

## Context

M2.3 lets someone join a workspace they are not yet a member of. That is the
first time Weave grants access to a caller who has no existing relationship
with the tenant, so the mechanism that carries the grant deserves a decision
rather than a default.

An invitation link is, by construction, a **bearer capability**: whoever holds
it can act on it. Links get forwarded, pasted into chat, and left in inboxes
for months. The question is what to store, what to check, and how much a
compromise of each part costs.

## Decision

**An invitation is treated as a credential, not a notification.** Concretely:

1. **256 bits from `crypto/rand`**, base64url-encoded unpadded. The token is
   the only thing standing between a stranger and a workspace, and it travels
   in a URL where it may be logged or shoulder-surfed.
2. **Only SHA-256 of the token is stored.** The plaintext is returned to the
   issuer once and never persisted.
3. **Single use.** Acceptance is a single conditional `UPDATE` that claims the
   invitation only if it is still pending, so concurrent accepts produce
   exactly one membership.
4. **Seven-day expiry**, so a link left in an inbox stops being a way in.
5. **The accepting account's email must match the invited address**,
   case-insensitively.
6. **Every unusable state returns one error.** Unknown, expired, revoked and
   already-accepted are indistinguishable to the caller.
7. **The token travels in a request body, never a query string.**

## Why a plain hash rather than a password hash

bcrypt or argon2 would be wrong here, and the reason is worth writing down
because "hash it properly" is the reflex.

Password hashing buys resistance to offline dictionary attack, which exists
because humans choose guessable passwords. An invitation token is 256 bits of
uniform randomness: there is no dictionary, and brute force is not a threat
model anyone can act on. What a work factor _would_ buy is a CPU cost on every
acceptance, and a denial-of-service lever pointed at our own API.

What the hash is actually for is narrower and still worth it: **a database
dump must not be a set of working invitations.** SHA-256 achieves that
completely. An attacker holding `token_hash` cannot present it — the
verification path hashes what it is given, so the stored value hashes to
something else. There is a test for exactly that.

## What an attacker gains at each level

- **Database read access:** the addresses invited, the roles offered, and who
  invited whom. They cannot accept anything. They already have far worse — the
  membership table — so this adds little.
- **The link, in transit or from a forwarded message:** nothing, unless they
  also control the invited email address at the identity provider. The email
  match is what makes forwarding safe.
- **Guessing:** 256 bits, and every failure answers identically, so there is no
  gradient to climb.

## Consequences

**Accepted:**

- **The token cannot be re-shown.** If the issuer loses it before sending, the
  only recourse is to revoke and re-issue. The UI must therefore make "copy
  this now" unmistakable — worse ergonomics, deliberately chosen.
- **The email match adds friction.** Someone whose GitHub account carries a
  different address from the one they were invited at will be refused, with an
  error explaining why. The alternative is a link that admits whoever opens it.
- **One error for four states makes support harder.** "This link is not valid"
  cannot tell an honest user whether they were too slow or the invitation was
  withdrawn. The audit log answers that for an operator, which is the right
  place for it.

**Deliberately deferred:**

- **Email delivery.** Weave does not send the invitation; the inviter shares
  the link. Adding a provider brings deliverability, bounce handling and
  templating, and belongs in its own unit.
- **Rate limiting on issue and accept.** Redis is already in the stack for
  exactly this, and it should arrive before invitations are exposed to the
  public internet. Not needed while the only users are the team.

**Revisit when:** invitations are reachable by unauthenticated internet
traffic, at which point acceptance needs rate limiting; or if an audit
concludes the address-match requirement blocks too many legitimate joins, in
which case the alternative is a confirmation step rather than dropping the
check.
