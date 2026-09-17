import "server-only";

import { auth } from "@clerk/nextjs/server";

import type { components, operations } from "./api-types.gen";

/**
 * Every response type here comes from `contracts/openapi/openapi.yaml`, via
 * `npm run contracts`. None is written by hand.
 *
 * That is a correctness property, not tidiness. A hand-written type is an
 * assertion about what the API returns that nothing checks — and during M3.1
 * the API gained `installation_id` on the repository response while the page
 * grouping by it compiled cleanly and grouped every repository under
 * `undefined`. Generated types make that a compile error.
 *
 * `make ci` regenerates and fails on drift, so this cannot quietly become a
 * hand-maintained file with a misleading name.
 */
type Schemas = components["schemas"];

/**
 * The 200/201 body of a named operation, straight from the contract.
 *
 * Response *envelopes* need this as much as the objects inside them. A
 * hand-written `{ repositories: Repository[] }` is still an unchecked
 * assertion even when `Repository` itself is generated — rename that key in
 * the contract and every caller keeps compiling while reading a field the API
 * no longer sends. Which is the exact failure this unit exists to remove, so
 * leaving the envelopes by hand would have closed half the hole and claimed
 * the whole one.
 */
type Ok<K extends keyof operations> = operations[K] extends {
  responses: infer R;
}
  ? R extends { 200: { content: { "application/json": infer B } } }
    ? B
    : R extends { 201: { content: { "application/json": infer B } } }
      ? B
      : never
  : never;

/** The authenticated user, as returned by `GET /v1/me`. */
export type User = Schemas["User"];

/** A member's position in a workspace. */
export type Role = Schemas["Role"];

/**
 * A workspace, with the caller's role and what that role permits.
 *
 * `permissions` is advisory — it exists so the UI can hide controls the caller
 * cannot use, rather than reimplementing the role matrix in TypeScript where
 * it would drift. The server enforces the same matrix regardless.
 */
export type Workspace = Schemas["Workspace"];

/** A workspace member. */
export type Member = Schemas["Member"];

/** A unit of work someone wants done. */
export type Task = Schemas["Task"];

/** Where a task sits between being written and being run. */
export type TaskStatus = Schemas["TaskStatus"];

/** An agent profile: identity and the settings currently in use. */
export type Agent = Schemas["Agent"];

/** The frozen settings an agent runs under. */
export type AgentVersion = Schemas["AgentVersion"];

/** The coding agent behind a profile. */
export type Provider = Schemas["Provider"];

/** Something a provider may or may not support. */
export type Capability = Schemas["Capability"];

/**
 * A run of an agent against a task.
 *
 * `branch_name` names a branch that does not exist yet — M5's workflow
 * creates it. `next_states` comes from the server's transition table rather
 * than a second copy of the state machine here, for the reason `permissions`
 * is served: two statements of one rule drift.
 */
export type Session = Schemas["Session"];

/** Where a session sits in its lifecycle. */
export type SessionState = Schemas["SessionState"];

/** One recorded move between states. */
export type SessionTransition = Schemas["SessionTransition"];

/** Someone taking part in a session. */
export type SessionParticipant = Schemas["SessionParticipant"];

/** An invitation to join a workspace. Never carries the token. */
export type Invitation = Schemas["Invitation"];

/**
 * A freshly issued invitation.
 *
 * `token` exists here and nowhere else — only its hash is stored, so it
 * cannot be fetched again. Surface it immediately.
 */
export type IssuedInvitation = Schemas["IssuedInvitation"];

/**
 * What the holder of a token may learn before accepting.
 *
 * No invited address: a forwarded token must not disclose who it was meant
 * for. The accept page shows the signed-in account instead.
 */
export type InvitationPreview = Schemas["InvitationPreview"];

/** A GitHub App installation bound to a workspace. */
export type Installation = Schemas["Installation"];

/**
 * A repository an installation grants.
 *
 * `granted` can be false. Withdrawn repositories are returned rather than
 * filtered out so the interface can say access was removed, instead of
 * silently losing something a member used yesterday.
 */
export type Repository = Schemas["Repository"];

/** What an installation can and cannot currently do. */
export type InstallationHealth = Schemas["InstallationHealth"];

/** The error envelope every failing endpoint returns. */
type ApiError = Schemas["Error"];

/**
 * A request outcome.
 *
 * Modelled as a discriminated union rather than throwing, so a caller cannot
 * render a page without having handled the failure case first.
 */
export type Result<T> =
  { ok: true; data: T } | { ok: false; status: number; message: string; requestId?: string };

/** How long a server-side API call may take before it is abandoned. */
const requestTimeoutMs = 5_000;

/**
 * The budget for calls that fan out to GitHub.
 *
 * Five seconds is right for endpoints that only touch our own API. These do
 * not: connecting an installation mints a token and lists every repository the
 * installation grants, which is several round trips to a third party and runs
 * to seconds on an account with a hundred repositories. Holding them to the
 * ordinary budget times out a request that is working perfectly well — and
 * worse, times it out *after* the binding has been written, so the page
 * reports failure for work that succeeded.
 */
const githubRequestTimeoutMs = 30_000;

function apiBaseUrl(): string {
  const url = process.env.API_BASE_URL;
  if (!url) {
    throw new Error("API_BASE_URL is not set. Copy .env.example to .env at the repository root.");
  }
  return url.replace(/\/$/, "");
}

/**
 * Fetch the authenticated user from the control-plane API.
 *
 * Runs on the server and forwards the Clerk session token as a bearer
 * credential. Keeping the call server-side means the token never reaches the
 * browser and no CORS configuration is required between the web application
 * and the API.
 */
export async function fetchCurrentUser(): Promise<Result<User>> {
  const { getToken } = await auth();
  const token = await getToken();

  if (!token) {
    // The middleware should have redirected before reaching here.
    return { ok: false, status: 401, message: "Not signed in." };
  }

  let response: Response;
  try {
    response = await fetch(`${apiBaseUrl()}/v1/me`, {
      headers: { Authorization: `Bearer ${token}` },
      // Identity is per-request state; caching it would serve one user's
      // profile to another.
      cache: "no-store",
      signal: AbortSignal.timeout(requestTimeoutMs),
    });
  } catch (error) {
    const reason =
      error instanceof Error && error.name === "TimeoutError"
        ? "The API did not respond in time."
        : "The API could not be reached.";
    return { ok: false, status: 0, message: reason };
  }

  if (!response.ok) {
    let message = `The API returned ${response.status}.`;
    let requestId: string | undefined;
    try {
      const body = (await response.json()) as Partial<ApiError>;
      if (body.message) message = body.message;
      requestId = body.request_id;
    } catch {
      // A non-JSON error body is itself unremarkable; keep the default.
    }
    return { ok: false, status: response.status, message, requestId };
  }

  return { ok: true, data: (await response.json()) as User };
}

/** List the workspaces the caller belongs to. */
export async function fetchWorkspaces(): Promise<Result<Workspace[]>> {
  const result = await apiRequest<Ok<"listWorkspaces">>("/v1/workspaces");
  return result.ok ? { ok: true, data: result.data.items } : result;
}

/** Create a workspace. The caller becomes its owner. */
export async function createWorkspace(name: string): Promise<Result<Workspace>> {
  return apiRequest<Workspace>("/v1/workspaces", {
    method: "POST",
    body: JSON.stringify({ name }),
  });
}

/** List a workspace's members. */
export async function fetchMembers(workspaceId: string): Promise<Result<Member[]>> {
  const result = await apiRequest<Ok<"listWorkspaceMembers">>(
    `/v1/workspaces/${workspaceId}/members`
  );
  return result.ok ? { ok: true, data: result.data.items } : result;
}

/** List a workspace's invitations, optionally narrowed to one status. */
export async function fetchInvitations(
  workspaceId: string,
  status?: Invitation["status"]
): Promise<Result<Invitation[]>> {
  const query = status ? `?status=${encodeURIComponent(status)}` : "";
  const result = await apiRequest<Ok<"listWorkspaceInvitations">>(
    `/v1/workspaces/${workspaceId}/invitations${query}`
  );
  return result.ok ? { ok: true, data: result.data.items } : result;
}

/** Invite someone to a workspace. The response carries the token, once. */
export async function issueInvitation(
  workspaceId: string,
  email: string,
  role: Role
): Promise<Result<IssuedInvitation>> {
  return apiRequest<IssuedInvitation>(`/v1/workspaces/${workspaceId}/invitations`, {
    method: "POST",
    body: JSON.stringify({ email, role }),
  });
}

/** Withdraw an outstanding invitation. */
export async function revokeInvitation(
  workspaceId: string,
  invitationId: string
): Promise<Result<void>> {
  return apiRequest<void>(`/v1/workspaces/${workspaceId}/invitations/${invitationId}`, {
    method: "DELETE",
  });
}

/**
 * Describe an invitation to its holder.
 *
 * POST, not GET: the token goes in the body, because a token in a query
 * string ends up in server logs, proxy logs and Referer headers.
 */
export async function previewInvitation(token: string): Promise<Result<InvitationPreview>> {
  return apiRequest<InvitationPreview>("/v1/invitations/preview", {
    method: "POST",
    body: JSON.stringify({ token }),
  });
}

/** Accept an invitation. */
export async function acceptInvitation(token: string): Promise<Result<Ok<"acceptInvitation">>> {
  return apiRequest<Ok<"acceptInvitation">>("/v1/invitations/accept", {
    method: "POST",
    body: JSON.stringify({ token }),
  });
}

/**
 * Start connecting GitHub, returning the URL to send the browser to.
 *
 * The server records which workspace this is for before answering, keyed by a
 * single-use value in the returned URL. That is the whole mechanism: GitHub
 * hands back an installation id and nothing identifying the workspace, so the
 * workspace cannot be taken from the callback.
 */
export async function beginGitHubInstall(
  workspaceId: string
): Promise<Result<Ok<"beginGitHubInstall">>> {
  return apiRequest<Ok<"beginGitHubInstall">>(`/v1/workspaces/${workspaceId}/github/install`, {
    method: "POST",
  });
}

/** Finish connecting GitHub after the browser returns from the setup URL. */
export async function completeGitHubInstall(
  state: string,
  installationId: number
): Promise<Result<Installation>> {
  return apiRequest<Installation>(
    "/v1/github/installations",
    { method: "POST", body: JSON.stringify({ state, installation_id: installationId }) },
    githubRequestTimeoutMs
  );
}

/** List a workspace's GitHub installations. */
export async function fetchInstallations(workspaceId: string): Promise<Result<Installation[]>> {
  const result = await apiRequest<Ok<"listGitHubInstallations">>(
    `/v1/workspaces/${workspaceId}/github/installations`
  );
  return result.ok ? { ok: true, data: result.data.installations } : result;
}

/** List a workspace's repositories, withdrawn ones included. */
export async function fetchRepositories(workspaceId: string): Promise<Result<Repository[]>> {
  const result = await apiRequest<Ok<"listWorkspaceRepositories">>(
    `/v1/workspaces/${workspaceId}/repositories`
  );
  return result.ok ? { ok: true, data: result.data.repositories } : result;
}

/** A workspace's tasks, newest first. */
export async function fetchTasks(workspaceId: string): Promise<Result<Task[]>> {
  const result = await apiRequest<Ok<"listTasks">>(`/v1/workspaces/${workspaceId}/tasks`);
  // `items` here and `sessions` below, because that is what the contract says.
  // The envelope key is inconsistent across the API — six operations use
  // `items` and four use a named key — which this unit is the first code to
  // consume all of and therefore the first to notice. Recorded in the tracker
  // rather than fixed here: aligning them touches ten operations, their
  // handlers and their tests, which is not a change to make inside a UI unit.
  return result.ok ? { ok: true, data: result.data.items } : result;
}

/** Create a task. */
export async function createTask(
  workspaceId: string,
  input: { title: string; body?: string; repository_id?: string; status?: TaskStatus }
): Promise<Result<Task>> {
  return apiRequest<Ok<"createTask">>(`/v1/workspaces/${workspaceId}/tasks`, {
    method: "POST",
    body: JSON.stringify(input),
  });
}

/**
 * Change some of a task's fields.
 *
 * A partial update: whatever is omitted is left alone, and only
 * `repository_id` accepts `null` — which is how a ready task returns to a
 * draft naming nothing. Sending `{}` is a no-op rather than a way to clear it.
 */
export async function updateTask(
  workspaceId: string,
  taskId: string,
  patch: { title?: string; body?: string; repository_id?: string | null; status?: TaskStatus }
): Promise<Result<Task>> {
  return apiRequest<Ok<"updateTask">>(`/v1/workspaces/${workspaceId}/tasks/${taskId}`, {
    method: "PATCH",
    body: JSON.stringify(patch),
  });
}

/** A workspace's agent profiles. */
export async function fetchAgents(workspaceId: string): Promise<Result<Agent[]>> {
  const result = await apiRequest<Ok<"listAgents">>(`/v1/workspaces/${workspaceId}/agents`);
  return result.ok ? { ok: true, data: result.data.items } : result;
}

/**
 * Create an agent profile and its first version.
 *
 * Returns both, because the response carries both: creating a profile also
 * creates the version a session would pin, and the version number is the
 * thing worth showing back. Asserting a bare `Agent` here compiled and read
 * `id` off an object that has no `id` — which is precisely the unchecked
 * assertion the generated types exist to prevent, so every wrapper in this
 * file states its operation rather than its shape.
 */
export async function createAgent(
  workspaceId: string,
  input: { name: string; provider: Provider; model: string; capabilities?: Capability[] }
): Promise<Result<{ agent: Agent; version: AgentVersion }>> {
  return apiRequest<Ok<"createAgent">>(`/v1/workspaces/${workspaceId}/agents`, {
    method: "POST",
    body: JSON.stringify(input),
  });
}

/** An agent's versions, newest first. */
export async function fetchAgentVersions(
  workspaceId: string,
  agentId: string
): Promise<Result<AgentVersion[]>> {
  const result = await apiRequest<Ok<"listAgentVersions">>(
    `/v1/workspaces/${workspaceId}/agents/${agentId}/versions`
  );
  return result.ok ? { ok: true, data: result.data.items } : result;
}

/** A workspace's sessions, newest first. */
export async function fetchSessions(workspaceId: string): Promise<Result<Session[]>> {
  const result = await apiRequest<Ok<"listSessions">>(`/v1/workspaces/${workspaceId}/sessions`);
  return result.ok ? { ok: true, data: result.data.sessions } : result;
}

/** One session. */
export async function fetchSession(
  workspaceId: string,
  sessionId: string
): Promise<Result<Session>> {
  return apiRequest<Ok<"getSession">>(`/v1/workspaces/${workspaceId}/sessions/${sessionId}`);
}

/** A session's state history, oldest first. */
export async function fetchSessionTransitions(
  workspaceId: string,
  sessionId: string
): Promise<Result<SessionTransition[]>> {
  const result = await apiRequest<Ok<"listSessionTransitions">>(
    `/v1/workspaces/${workspaceId}/sessions/${sessionId}/transitions`
  );
  return result.ok ? { ok: true, data: result.data.transitions } : result;
}

/** Who is in a session. */
export async function fetchSessionParticipants(
  workspaceId: string,
  sessionId: string
): Promise<Result<SessionParticipant[]>> {
  const result = await apiRequest<Ok<"listSessionParticipants">>(
    `/v1/workspaces/${workspaceId}/sessions/${sessionId}/participants`
  );
  return result.ok ? { ok: true, data: result.data.participants } : result;
}

/**
 * Start a session from a ready task.
 *
 * `idempotencyKey` should be sent, and should be **stable across retries of
 * the same submission**. Without one a lost response cannot be distinguished
 * from a request that never arrived, and retrying creates a second session —
 * which becomes a second workflow and a second branch once the session
 * workflow exists. A key regenerated per attempt is worse than none, because
 * it looks like protection: that is the mistake M3.3 made with branch names.
 */
export async function createSession(
  workspaceId: string,
  input: { task_id: string; agent_version_id: string; base_branch?: string },
  idempotencyKey?: string
): Promise<Result<Session>> {
  return apiRequest<Ok<"createSession">>(`/v1/workspaces/${workspaceId}/sessions`, {
    method: "POST",
    body: JSON.stringify(input),
    ...(idempotencyKey ? { headers: { "Idempotency-Key": idempotencyKey } } : {}),
  });
}

/** Check one installation against GitHub. */
export async function fetchInstallationHealth(
  workspaceId: string,
  installationId: string
): Promise<Result<InstallationHealth>> {
  return apiRequest<InstallationHealth>(
    `/v1/workspaces/${workspaceId}/github/installations/${installationId}/health`,
    {},
    githubRequestTimeoutMs
  );
}

/** Pull the current repository set from GitHub. */
export async function reconcileInstallation(
  workspaceId: string,
  installationId: string
): Promise<Result<void>> {
  return apiRequest<void>(
    `/v1/workspaces/${workspaceId}/github/installations/${installationId}/reconcile`,
    { method: "POST" },
    githubRequestTimeoutMs
  );
}

/**
 * Issue a request to the control-plane API with the caller's session token.
 *
 * Server-side only, so the token never reaches the browser and no CORS
 * configuration is needed.
 */
async function apiRequest<T>(
  path: string,
  init: RequestInit = {},
  timeoutMs: number = requestTimeoutMs
): Promise<Result<T>> {
  const { getToken } = await auth();
  const token = await getToken();

  if (!token) {
    return { ok: false, status: 401, message: "Not signed in." };
  }

  let response: Response;
  try {
    response = await fetch(`${apiBaseUrl()}${path}`, {
      ...init,
      headers: {
        Authorization: `Bearer ${token}`,
        ...(init.body ? { "Content-Type": "application/json" } : {}),
        ...init.headers,
      },
      cache: "no-store",
      signal: AbortSignal.timeout(timeoutMs),
    });
  } catch (error) {
    const reason =
      error instanceof Error && error.name === "TimeoutError"
        ? "The API did not respond in time."
        : "The API could not be reached.";
    return { ok: false, status: 0, message: reason };
  }

  if (!response.ok) {
    let message = `The API returned ${response.status}.`;
    let requestId: string | undefined;
    try {
      const body = (await response.json()) as Partial<ApiError>;
      if (body.message) message = body.message;
      requestId = body.request_id;
    } catch {
      // A non-JSON error body is unremarkable; keep the default.
    }
    return { ok: false, status: response.status, message, requestId };
  }

  if (response.status === 204) {
    return { ok: true, data: undefined as T };
  }
  return { ok: true, data: (await response.json()) as T };
}
