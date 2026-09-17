"use server";

import { revalidatePath } from "next/cache";

import {
  createAgent,
  createSession,
  createTask,
  updateTask,
  type Capability,
  type Provider,
} from "@/lib/api";

/** What each form reports back. */
export type FormState = { error?: string; created?: string; note?: string };

/**
 * Create a task.
 *
 * The body is passed through exactly as typed. Nothing trims, escapes or
 * re-encodes it: the API stores and returns it unaltered, and altering it here
 * would make what the user sees disagree with what is stored.
 */
export async function createTaskAction(
  workspaceId: string,
  _previous: FormState,
  formData: FormData
): Promise<FormState> {
  const title = String(formData.get("title") ?? "").trim();
  if (title === "") return { error: "Give the task a title." };

  const body = String(formData.get("body") ?? "");
  const repositoryId = String(formData.get("repository_id") ?? "");
  const ready = formData.get("ready") === "on";

  // A ready task must name a repository — the database enforces it, and
  // saying so here means the caller gets a sentence rather than a constraint
  // violation relayed from two layers down.
  if (ready && repositoryId === "") {
    return { error: "Choose a repository before marking the task ready." };
  }

  const result = await createTask(workspaceId, {
    title,
    body,
    ...(repositoryId ? { repository_id: repositoryId } : {}),
    status: ready ? "ready" : "draft",
  });
  if (!result.ok) return { error: result.message };

  revalidatePath(`/workspaces/${workspaceId}/tasks`);
  return { created: result.data.id };
}

/**
 * Mark a draft task ready, or send a ready one back to draft.
 *
 * Shaped as a form action — `(previous, formData)` after the bound arguments —
 * so the caller can bind it into `useActionState` rather than calling it from
 * a click handler. Next only re-renders the route when the result comes back
 * as an action response, and a closure around the action does not produce one.
 */
export async function setTaskReadyAction(
  workspaceId: string,
  taskId: string,
  ready: boolean,
  _previous: FormState,
  _formData: FormData
): Promise<FormState> {
  // Only `status` is sent. This is a PATCH and everything omitted is left
  // alone, so the title and the body are untouched — which is the point of
  // the endpoint being a PATCH rather than a replace.
  const result = await updateTask(workspaceId, taskId, { status: ready ? "ready" : "draft" });
  if (!result.ok) return { error: result.message };

  revalidatePath(`/workspaces/${workspaceId}/tasks`);
  return {};
}

/** Create an agent profile and its first version. */
export async function createAgentAction(
  workspaceId: string,
  _previous: FormState,
  formData: FormData
): Promise<FormState> {
  const name = String(formData.get("name") ?? "").trim();
  const provider = String(formData.get("provider") ?? "") as Provider;
  const model = String(formData.get("model") ?? "").trim();

  if (name === "") return { error: "Give the agent a name." };
  if (!provider) return { error: "Choose a provider." };
  if (model === "") return { error: "Name the model this agent uses." };

  // The closed set the API also enforces. Sent only when chosen, so an agent
  // declaring nothing is distinguishable from one declaring an empty list.
  const capabilities = formData.getAll("capabilities").map(String) as Capability[];

  const result = await createAgent(workspaceId, {
    name,
    provider,
    model,
    ...(capabilities.length > 0 ? { capabilities } : {}),
  });
  if (!result.ok) return { error: result.message };

  revalidatePath(`/workspaces/${workspaceId}/agents`);
  // The version number is worth saying back: editing this profile later writes
  // a second version rather than changing this one, and the count is the
  // clearest way to see that happen.
  return {
    created: result.data.agent.id,
    note: `${result.data.agent.name} created at version ${result.data.version.version}.`,
  };
}

/** Start a session from a ready task and an agent version. */
export async function createSessionAction(
  workspaceId: string,
  _previous: FormState,
  formData: FormData
): Promise<FormState> {
  const taskId = String(formData.get("task_id") ?? "");
  const agentVersionId = String(formData.get("agent_version_id") ?? "");
  const baseBranch = String(formData.get("base_branch") ?? "").trim();

  if (taskId === "") return { error: "Choose a task." };
  if (agentVersionId === "") return { error: "Choose an agent." };

  // Carried in the form rather than generated here. Generated here it would be
  // new on every attempt, which is exactly the failure it is meant to prevent:
  // a retry after a lost response would create a second session while looking
  // protected.
  const idempotencyKey = String(formData.get("idempotency_key") ?? "") || undefined;

  const result = await createSession(
    workspaceId,
    {
      task_id: taskId,
      agent_version_id: agentVersionId,
      ...(baseBranch ? { base_branch: baseBranch } : {}),
    },
    idempotencyKey
  );
  if (!result.ok) return { error: result.message };

  revalidatePath(`/workspaces/${workspaceId}/sessions`);
  return { created: result.data.id };
}
