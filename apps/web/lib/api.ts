import "server-only";

import { auth } from "@clerk/nextjs/server";

/**
 * The authenticated user, as returned by `GET /v1/me`.
 *
 * Hand-written for this one endpoint. Once the API surface grows past a
 * couple of routes these types come from the generated OpenAPI client
 * (`contracts/openapi/openapi.yaml`) rather than being maintained here.
 */
export type User = {
  id: string;
  email: string;
  display_name?: string;
  avatar_url?: string;
};

/** The error envelope every failing endpoint returns. */
type ApiError = {
  code: string;
  message: string;
  request_id: string;
};

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

function apiBaseUrl(): string {
  const url = process.env.API_BASE_URL;
  if (!url) {
    throw new Error("API_BASE_URL is not set. Copy .env.example to .env at the repository root.");
  }
  return url.replace(/\/$/, "");
}

/** A member's position in a workspace. */
export type Role = "owner" | "admin" | "developer" | "viewer";

/**
 * A workspace, with the caller's role and what that role permits.
 *
 * `permissions` is advisory — it exists so the UI can hide controls the caller
 * cannot use, rather than reimplementing the role matrix in TypeScript where
 * it would drift. The server enforces the same matrix regardless.
 */
export type Workspace = {
  id: string;
  slug: string;
  name: string;
  version: number;
  role: Role;
  permissions: string[];
  created_at: string;
};

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

/**
 * Issue a request to the control-plane API with the caller's session token.
 *
 * Server-side only, so the token never reaches the browser and no CORS
 * configuration is needed.
 */
async function apiRequest<T>(path: string, init: RequestInit = {}): Promise<Result<T>> {
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
      // A non-JSON error body is unremarkable; keep the default.
    }
    return { ok: false, status: response.status, message, requestId };
  }

  if (response.status === 204) {
    return { ok: true, data: undefined as T };
  }
  return { ok: true, data: (await response.json()) as T };
}
