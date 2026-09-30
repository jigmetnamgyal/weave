"use server";

import { revalidatePath } from "next/cache";

import { addEgressHost, removeEgressHost } from "@/lib/api";

/**
 * What the egress forms report back.
 *
 * `hostname` is the value the user typed, returned on failure so the form can
 * keep it rather than make them type it again.
 */
export type EgressFormState = {
  error?: string;
  hostname?: string;
  added?: string;
  removed?: string;
};

/**
 * Add a host a workspace's runners may reach.
 *
 * A pass-through: the API validates the hostname, checks `workspace:manage`
 * at the moment of the change, enforces the limit and writes the audit record.
 * Nothing here decides anything the API does not decide again.
 */
export async function addEgressHostAction(
  workspaceId: string,
  _previous: EgressFormState,
  formData: FormData
): Promise<EgressFormState> {
  const hostname = String(formData.get("hostname") ?? "").trim();
  if (hostname === "") return { error: "Enter a hostname, such as docs.example.com." };

  // Carried in the form, one per submission, so a retry after a lost response
  // is answered with the first result instead of adding the host twice.
  const idempotencyKey = String(formData.get("idempotency_key") ?? "") || undefined;

  const result = await addEgressHost(workspaceId, hostname, idempotencyKey);
  if (!result.ok) return { error: result.message, hostname };

  revalidatePath(`/workspaces/${workspaceId}/settings/egress`);
  return { added: result.data.hostname };
}

/**
 * Remove a host from the list new sessions start with.
 *
 * Shaped as a form action and bound into `useActionState`, so Next re-renders
 * the route from the action response — a closure around the call would not
 * (the M3.1 and M4.3 lesson).
 */
export async function removeEgressHostAction(
  workspaceId: string,
  egressHostId: string,
  _previous: EgressFormState,
  formData: FormData
): Promise<EgressFormState> {
  const idempotencyKey = String(formData.get("idempotency_key") ?? "") || undefined;

  const result = await removeEgressHost(workspaceId, egressHostId, idempotencyKey);
  if (!result.ok) return { error: result.message };

  revalidatePath(`/workspaces/${workspaceId}/settings/egress`);
  return { removed: egressHostId };
}
