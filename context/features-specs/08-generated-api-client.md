Read `CLAUDE.md` before starting

We're generating the web application's API types from the OpenAPI contract
(Unit M3.2), so that the contract and the code that calls it cannot
drift apart silently.

`context/code-standards.md` has required this since M0 — "do not
duplicate API response types manually; generate them from OpenAPI" — and
`apps/web/lib/api.ts` has hand-written every response type since M2.1.
Fifteen exported functions across eleven call sites. Review raised it on
PR #6 and the gap was accepted there as a documented exception, on the
condition that it is closed before the surface grows again.

That condition is why this is its own unit and comes first. M3.3 adds
cloning, branches, commits and pull requests; doing this afterwards means
migrating a third layer of hand-written helpers instead of two.

## What is actually wrong today

Not the shape of `apps/web/lib/api.ts`. It is one module, not the
"scattered ad hoc `fetch` helpers" the standard warns against, and its
`Result<T>` discriminated union is a deliberate and good design — a
caller cannot render a page without having handled the failure case,
because the type will not let them.

What is wrong is that every type in it is typed by hand from a contract
the compiler never checks it against. `Repository`, `Installation`,
`Workspace` and the rest are assertions about what the API returns, and
nothing verifies them. A field renamed in Go compiles cleanly on both
sides and fails at runtime, in a browser, as `undefined`.

**That failure has already happened once in this codebase.** During M3.1
the API gained `installation_id` on the repository response; the page
grouping repositories by it silently grouped every repository under
`undefined`, and every connected account rendered as empty. It took a
build-timestamp comparison to find. Generated types would have made it a
compile error.

## Decisions this unit locks in

- **Generate types, not a client.** `openapi-typescript` produces types
  from the contract and nothing else — no runtime, no request layer, no
  opinion about error handling. The existing `Result<T>` wrapper stays.
- **Keep `Result<T>`.** A generated fetch client that throws on non-2xx
  would replace a design that makes failure unignorable with one that
  makes it easy to forget. The standard's concern is duplicated types and
  scattered callers; one typed module generated from the contract meets
  it. Record this reading in the unit's notes rather than leaving it
  implicit — it is a deliberate interpretation, not an oversight.
- **A drift check in CI, or this does not hold.** Committed output plus
  a regenerate-and-diff gate, exactly as `sqlc-check` already does for
  the database layer. Without it the generated file becomes another
  hand-maintained file with a misleading name.
- **The contract is not yet verified against the Go implementation.**
  Generated types are only as true as the contract they come from, and
  nothing today checks that the contract matches what the handlers
  actually serve. This unit does not close that, and must not pretend to.
  It closes the cheap half — see below.

## The work

- Add `openapi-typescript` and a `contracts` generation script. Output
  goes to a committed file under `apps/web/lib/` with a header saying it
  is generated and must not be edited.
- Add `contracts-check` to the Makefile and to `make ci`, mirroring
  `sqlc-check`: regenerate, fail if the working tree differs.
- Replace every hand-written response type in `apps/web/lib/api.ts` with
  the generated equivalent. The exported names callers use should not
  change — this is a type-source change, not an API change for the
  eleven call sites.
- Where a generated type and a hand-written one disagree, **the
  disagreement is the finding**. Do not quietly adopt the generated
  shape: work out which side is wrong, fix that side, and say so. One of
  them describes what the API really returns and the other does not.

## Route coverage, the cheap half of contract verification

Nothing stops a Go handler being added without the contract learning
about it — which is exactly what happened in M3.1, where seven endpoints
existed for a week with no description.

Add a test that compares the routes registered on the API's mux against
the paths in `contracts/openapi/openapi.yaml`, failing on either
direction:

- a route the contract does not describe
- a contract path no route serves

This is cheap and catches the common case. It says nothing about whether
the *shapes* match, which is the expensive half and deliberately out of
scope here. Name that limitation in the test's own comment so nobody
reads a passing check as more than it is.

## Tests

- The drift check fails when the contract changes and the generated file
  is not regenerated. Prove it by changing one and not the other.
- The route-coverage test fails in both directions, proven the same way.
- Every existing web test still passes, and `npm run typecheck` is clean
  — the migration is a type-source change, so a compile error is the
  expected signal if a hand-written type was wrong.

### Check when done

- `apps/web/lib/api.ts` declares no response types of its own; all come
  from the generated file.
- `make ci` regenerates and fails on drift.
- The route-coverage test passes, and fails when a route is added without
  the contract.
- Any disagreement found between a hand-written type and the contract is
  recorded in the tracker with which side was wrong.
- `make ci` and `make test-integration` pass.
