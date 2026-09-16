Read `CLAUDE.md` before starting

We're giving tasks, agent profiles and sessions a surface in the browser
(Unit M4.3), because three units have now shipped without one and M4
cannot be checked by a person.

## Why this unit exists

The contract describes **35 endpoints. The browser can reach 15.** From a
signed-in session you can create a workspace, invite people and connect
GitHub. You cannot create a task, define an agent, or start a session —
the entire output of M3.3, M4.1 and M4.2 is reachable only by `curl`.

That is not a cosmetic gap, and the evidence is in this repository's own
record. The end-to-end browser walk on 2026-09-11 found **four defects no
automated test had caught**: an install callback that reported failure
for work that had succeeded, a repository list with a hard-coded
`connected[0]` so only one of two installations could ever be refreshed,
a sync action bound in a closure so `revalidatePath` invalidated a cache
the router was never told about, and a failed fetch swallowed into "no
repositories yet" — a claim about GitHub when the cause was ours. Every
one of those was found by clicking.

Three units have since shipped with that check unavailable. This unit
makes it available again. **It is a verification unit, and its deliverable
is a walk a person can complete**, not a feature.

## What this is not

- **Not the session room.** Live events, streaming and reconnect are M6.
  A session here is a row you can see, not a thing you can watch.
- **Not a design showcase.** `context/ui-context.md` governs; use the
  existing cards, forms and semantic tokens. Nothing hardcodes a colour.
- **Not new API surface.** Every endpoint this needs exists. If something
  seems to be missing, that is a finding worth recording, not a licence
  to add an endpoint in a UI unit.
- **Not a web test runner.** The tracker's standing note says one arrives
  with the first stateful UI logic — reducers, event merging — and a
  create form is not that. Verification here is the browser walk, which
  is the point of the unit.

## The walk this has to make possible

One person, signed in, without leaving the browser:

1. Sees the repositories their workspace has connected.
2. Writes a task, chooses the repository it concerns, marks it ready.
3. Defines an agent profile — provider, model, capabilities — and sees
   the version number it was given.
4. Starts a session from the ready task and that agent.
5. Sees the session sitting in `queued`, with its branch name, its
   participant, and its one state transition.

Step 5 is the one that matters, because it is the first time M4.2's work
is visible to anyone who did not write it.

## Things the surface must get right

**Permissions are an affordance, never a control.** Hide what the caller
cannot do — a viewer does not see a "New task" button — but the API
enforces the same matrix regardless, and the UI must handle a 403 it did
not expect rather than assuming its own hiding was sufficient. The
`permissions` array on the workspace is served for exactly this and is
already used by the members page; follow it rather than reimplementing
the matrix in TypeScript, where it would drift.

**Every type comes from the generated contract.** `apps/web/lib/api.ts`
has no hand-written response shapes and must not acquire any — including
envelopes. `{ sessions: Session[] }` written by hand is still an
unchecked assertion even when `Session` is generated, which is the half-
closed hole M3.2's review found.

**A task body is untrusted input and stays that way.** It is rendered as
text. Nothing interpolates it into markup, a URL, or a template.

**The branch name is not a branch.** A session's `branch_name` names a
branch that **does not exist yet** — M5's workflow creates it. Say so
where it is shown, or the first person to click it in GitHub will file a
bug against the wrong unit.

**What a session offers comes from `next_states`**, which the API serves
from the transition table. Do not write a second copy of the state
machine in the browser; that is the rule the permission matrix already
follows, for the same reason.

## Carried forward, and worth reading before starting

**From the 2026-09-11 walk, on server actions:** an action wrapped in a
client closure rather than bound left `revalidatePath` invalidating the
server cache while the router was never told, so a working button looked
dead. Bind actions; do not wrap them.

**From the same walk, on empty states:** a failed fetch was rendered as
"no repositories yet", which states something about GitHub when the fact
is that our request failed. An empty list and a failed request are
different sentences and must not share one.

**From M3.1, on latency:** listing 113 repositories took 3.3 seconds and
blew a five-second client budget that also failed *after* the write had
succeeded. Any request here that can be slow needs its failure path
thought about before it is written, not after it is seen.

**From M4.2, on comments:** the first version of its store comments
credited a row lock for safety that a different lock was providing. When
writing why something is correct, check that it is the reason.

### Check when done

- The five-step walk above completes in a browser against the local
  stack, and the session appears in `queued`.
- A viewer sees no create controls on any of the three surfaces, and the
  API refuses them if they are reached another way.
- A task body containing markup, shell metacharacters and unicode
  survives the round trip and renders as text.
- A failed request and an empty list read differently.
- `make ci` passes, including `contracts-check` and `typecheck`.
- **Separately, and by the operator:** M3.3's branch creation has still
  never run against a real repository. It is not wired into this
  surface — M5's workflow is what will call it, and adding a button now
  would build UI for something about to become automatic. Verify it once
  by hand against `jigmetnamgyal/thuenlam` and record the result, so M5
  is not debugging the workflow and the branch code at the same time.
