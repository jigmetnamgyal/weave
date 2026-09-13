"use server";

import { redirect } from "next/navigation";
import { revalidatePath } from "next/cache";

import { beginGitHubInstall, reconcileInstallation } from "@/lib/api";

/** What the connect button reports back when it cannot proceed. */
export type ConnectState = { error?: string };

/**
 * Start connecting GitHub.
 *
 * The URL is built by the server, not here, because it carries a single-use
 * value that records which workspace the installation will belong to. Building
 * it in the browser would put the workspace under the caller's control, which
 * is exactly what the value exists to prevent.
 */
export async function connectGitHubAction(
  workspaceId: string,
  _previous: ConnectState,
  _formData: FormData
): Promise<ConnectState> {
  const result = await beginGitHubInstall(workspaceId);
  if (!result.ok) {
    return { error: result.message };
  }
  // Outside the try/catch shape above: redirect signals by throwing, so it
  // must be the last thing and must not be swallowed.
  redirect(result.data.install_url);
}

/** What the sync control reports back. */
export type SyncState = { error?: string; synced?: boolean };

/**
 * Pull the current repository set from GitHub.
 *
 * The signature ends in `(previous, formData)` so this can be passed to
 * `useActionState` as a bound server action rather than called from inside a
 * client closure. That distinction is not cosmetic: calling a server action
 * from a wrapper function runs it, and `revalidatePath` does invalidate the
 * server cache — but the client router is never told, so the page keeps
 * rendering what it already had. The work succeeds and the screen does not
 * change, which is indistinguishable from a button that does nothing.
 */
export async function refreshRepositoriesAction(
  workspaceId: string,
  installationId: string,
  _previous: SyncState,
  _formData: FormData
): Promise<SyncState> {
  const result = await reconcileInstallation(workspaceId, installationId);
  if (!result.ok) {
    return { error: result.message };
  }
  revalidatePath(`/workspaces/${workspaceId}`);
  return { synced: true };
}
