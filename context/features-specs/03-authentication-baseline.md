Read `CLAUDE.md` before starting

We're adding the authentication baseline (Unit M2.1): a developer
signs in with GitHub through Clerk, and the Go API verifies that
session and resolves it to an internal user record.

This does not include workspaces, membership, roles, invitations, or
the authorization matrix. Those are separate units.

## Decisions this unit locks in

- Clerk is the identity provider, sitting behind the OIDC/JWT
  identity boundary described in `architecture.md`.
- Clerk owns authentication only. Workspaces, membership and every
  permission decision live in PostgreSQL. Do not use Clerk
  Organizations — two sources of truth for authorization is how
  tenant isolation breaks.
- Clerk's GitHub OAuth establishes identity only. Repository access
  is the GitHub App (M3) and must not reuse this token.
- Record an ADR for the identity provider and for the migration tool
  chosen below.

## Database foundation

- Add `goose` as the migration runner, with `make migrate-up` and
  `make migrate-down`.
- Add `sqlc` with queries in `db/queries/` and generated output
  committed.
- The first migration creates `users`:
  - `id` UUIDv7 primary key
  - `external_id` text, unique — the Clerk subject
  - `email` citext, `display_name` and `avatar_url` nullable
  - `created_at` and `updated_at` timestamptz
- The Clerk subject is never a primary key or a foreign key.

## Identity boundary (Go)

- Define the port in `internal/application`: a verifier that turns a
  raw bearer token into a verified identity (subject, email, expiry).
- Implement it for Clerk in `internal/adapters`, validating issuer,
  audience, signature, expiry and token type against cached JWKS.
- Cache JWKS with a bounded refresh. Never fetch per request, and
  never fail open when the fetch fails.
- User entity and its invariants live in `internal/domain`.

## API

- Add authentication middleware that rejects a missing, malformed,
  expired or untrusted token with 401 and the standard error shape.
- Deny by default: a route is protected unless explicitly declared
  public. `/health/live` and `/health/ready` stay public.
- Provision the user just in time on first authenticated request,
  keyed on the Clerk subject with a unique constraint so concurrent
  first requests cannot create duplicates. Clerk webhooks are
  deferred.
- Add `GET /v1/me` returning the internal user id, email and display
  name. Never return the Clerk subject.
- Document `/v1/me` and the error shape in `contracts/openapi/`.
  Generating the TypeScript client is deferred until the browser
  calls the API directly.

## Web

- Add Clerk to `apps/web` with GitHub as the only sign-in method.
- `CLERK_SECRET_KEY` is server-only. Only the publishable key may
  carry the `NEXT_PUBLIC_` prefix.
- Add a sign-in route, and a protected route that redirects to
  sign-in when unauthenticated.
- The protected route is a Server Component that calls `GET /v1/me`
  server-side with the session token and renders the result. Keeping
  the call server-side means this unit needs no CORS.
- Use the existing theme tokens and shadcn primitives. Do not edit
  `components/ui/*`.

## Environment

- Add the Clerk variables to `.env.example` with placeholder values,
  and to the API's required set where the API needs them.
- Startup must still fail fast and name every missing variable at
  once.

## Tests

- Verifier: valid token, expired, wrong issuer, wrong audience,
  tampered signature, malformed, and missing.
- Middleware: 401 for every rejection case, pass-through on valid,
  and public routes reachable without a token.
- Provisioning: a first request creates one user; concurrent first
  requests create exactly one; later requests reuse it.
- `/v1/me` response never contains the Clerk subject.
- Use a signed test key, not a real Clerk token or live network call.

### Check when done

- `make dev` still starts everything and both health probes pass.
- Signing in with GitHub lands on the protected route, showing the
  internal user id and email returned by the Go API.
- Signing out and revisiting the protected route redirects to
  sign-in.
- `GET /v1/me` without a token returns 401, and so does an expired
  or tampered token.
- The same GitHub account signing in twice yields exactly one row in
  `users`.
- `make ci` passes, including the new tests.
- No secret appears in `.env.example` or behind any `NEXT_PUBLIC_`
  variable.
