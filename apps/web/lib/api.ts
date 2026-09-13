import "server-only";

import { auth } from "@clerk/nextjs/server";

import type { components } from "./api-types.gen";

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

type ListResponse<T> = { items: T[] };

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
  const result = await apiRequest<ListResponse<Workspace>>("/v1/workspaces");
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
  const result = await apiRequest<ListResponse<Member>>(`/v1/workspaces/${workspaceId}/members`);
  return result.ok ? { ok: true, data: result.data.items } : result;
}

/** List a workspace's invitations, optionally narrowed to one status. */
export async function fetchInvitations(
  workspaceId: string,
  status?: Invitation["status"]
): Promise<Result<Invitation[]>> {
  const query = status ? `?status=${encodeURIComponent(status)}` : "";
  const result = await apiRequest<ListResponse<Invitation>>(
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
export async function acceptInvitation(
  token: string
): Promise<Result<{ workspace_id: string; role: Role }>> {
  return apiRequest<{ workspace_id: string; role: Role }>("/v1/invitations/accept", {
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
): Promise<Result<{ install_url: string }>> {
  return apiRequest<{ install_url: string }>(`/v1/workspaces/${workspaceId}/github/install`, {
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
  const result = await apiRequest<{ installations: Installation[] }>(
    `/v1/workspaces/${workspaceId}/github/installations`
  );
  return result.ok ? { ok: true, data: result.data.installations } : result;
}

/** List a workspace's repositories, withdrawn ones included. */
export async function fetchRepositories(workspaceId: string): Promise<Result<Repository[]>> {
  const result = await apiRequest<{ repositories: Repository[] }>(
    `/v1/workspaces/${workspaceId}/repositories`
  );
  return result.ok ? { ok: true, data: result.data.repositories } : result;
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
