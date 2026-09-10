"use server";

import { revalidatePath } from "next/cache";
import { redirect } from "next/navigation";

import { createWorkspace } from "@/lib/api";

/** What the create-workspace form reports back to the client component. */
export type CreateWorkspaceState = {
  error?: string;
};

/**
 * Create a workspace on behalf of the signed-in user.
 *
 * This is a thin pass-through to the control-plane API, which owns the
 * decision. Server Actions are deliberately not an alternative API: the
 * authorization, validation and audit trail all live behind `/v1/workspaces`,
 * and this only forwards the caller's session to it.
 */
export async function createWorkspaceAction(
  _previous: CreateWorkspaceState,
  formData: FormData
): Promise<CreateWorkspaceState> {
  const name = String(formData.get("name") ?? "").trim();
  if (name === "") {
    return { error: "Enter a name for the workspace." };
  }

  const result = await createWorkspace(name);
  if (!result.ok) {
    return { error: result.message };
  }

  revalidatePath("/dashboard");
  // The acceptance criterion is that a new user "lands in" their workspace,
  // not merely that it appears in a list. redirect() throws a control-flow
  // signal Next.js handles, so it must stay outside any try/catch.
  redirect(`/workspaces/${result.data.id}`);
}
