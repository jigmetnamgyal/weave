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

/** Pull the current repository set from GitHub. */
export async function refreshRepositoriesAction(
  workspaceId: string,
  installationId: string
): Promise<{ error?: string }> {
  const result = await reconcileInstallation(workspaceId, installationId);
  if (!result.ok) {
    return { error: result.message };
  }
  revalidatePath(`/workspaces/${workspaceId}`);
  return {};
}
