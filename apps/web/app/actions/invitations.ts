"use server";

import { revalidatePath } from "next/cache";

import { acceptInvitation, issueInvitation, revokeInvitation, type Role } from "@/lib/api";

/** What the invite form reports back. */
export type InviteState = {
  error?: string;
  /**
   * The invitation link, returned once.
   *
   * Only the token's hash is stored, so this cannot be fetched again. The form
   * shows it immediately and says so.
   */
  link?: string;
  email?: string;
};

/** Issue an invitation and return the link for the inviter to share. */
export async function inviteMemberAction(
  workspaceId: string,
  _previous: InviteState,
  formData: FormData
): Promise<InviteState> {
  const email = String(formData.get("email") ?? "").trim();
  const role = String(formData.get("role") ?? "") as Role;

  if (email === "") return { error: "Enter an email address." };
  if (!role) return { error: "Choose a role." };

  const result = await issueInvitation(workspaceId, email, role);
  if (!result.ok) return { error: result.message };

  revalidatePath(`/workspaces/${workspaceId}/members`);

  // Built here rather than by the API: the API has no idea what host the web
  // application is served from.
  const base = process.env.NEXT_PUBLIC_APP_URL ?? "http://localhost:3000";
  return { link: `${base}/invitations/${result.data.token}`, email };
}

/** Withdraw an outstanding invitation. */
export async function revokeInvitationAction(
  workspaceId: string,
  invitationId: string
): Promise<{ error?: string }> {
  const result = await revokeInvitation(workspaceId, invitationId);
  if (!result.ok) return { error: result.message };

  revalidatePath(`/workspaces/${workspaceId}/members`);
  return {};
}

/** Accept an invitation on behalf of the signed-in user. */
export async function acceptInvitationAction(
  token: string
): Promise<{ error?: string; workspaceId?: string }> {
  const result = await acceptInvitation(token);
  if (!result.ok) return { error: result.message };

  revalidatePath("/dashboard");
  return { workspaceId: result.data.workspace_id };
}
