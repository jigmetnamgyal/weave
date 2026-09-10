# Clerk instance configuration

Settings that Weave depends on, kept here so they are reviewable and
reproducible across instances instead of living only in the Clerk dashboard.

Only the settings we deliberately manage are stored. Pulling the whole
configuration document would bake in every Clerk default and turn each
upstream change into a diff.

## session-claims.json

Clerk's default session token is intentionally minimal — subject, issuer and
timestamps, nothing else — because it travels in a cookie on every request.

The Go API provisions a user record on first authenticated request, straight
from the token, and that record needs an email. Without these claims the API
correctly rejects an otherwise-valid token, because a user cannot be created
from it.

The alternatives were to give the API a Clerk secret key so it could call the
Backend API during provisioning, or to accept incomplete user rows. Adding the
claims keeps the secret key out of the API entirely — signature verification
needs only public keys — and keeps `users.email` non-null.

## Applying

Check what would change:

```bash
clerk config patch --file infra/clerk/session-claims.json --dry-run
```

Apply it:

```bash
clerk config patch --file infra/clerk/session-claims.json
```

Target a different instance with `--instance prod`.

**A new Clerk instance needs this applied before sign-in works.** It is not
carried over when an application is created, and its absence shows up as a
`401` with `token carries no email claim` in the API logs.

## Verifying

```bash
clerk config pull
```

Compare the `session` key against this file.
