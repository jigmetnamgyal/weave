"use client";

import { useActionState } from "react";
import { useFormStatus } from "react-dom";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { createSessionAction, type FormState } from "@/app/actions/sessions";
import type { Agent, Task } from "@/lib/api";

const initialState: FormState = {};

/**
 * Start a session.
 *
 * Only ready tasks and only agents that have a version are offered — a draft
 * task has no repository to cut a branch in, and an agent with no version has
 * no settings to pin. Both would be refused by the API; not offering them
 * means the refusal is never reached rather than merely explained.
 */
export function StartSessionForm({
  workspaceId,
  readyTasks,
  agents,
}: {
  workspaceId: string;
  readyTasks: Task[];
  agents: Agent[];
}) {
  const [state, formAction] = useActionState(
    createSessionAction.bind(null, workspaceId),
    initialState
  );

  const runnable = agents.filter((agent) => agent.current_version_id);

  if (readyTasks.length === 0 || runnable.length === 0) {
    return (
      <p className="text-muted-foreground text-sm">
        {readyTasks.length === 0
          ? "No task is ready yet. A session needs a task that names the repository it runs against."
          : "No agent has settings yet, so there is nothing for a session to pin."}
      </p>
    );
  }

  return (
    <form action={formAction} className="space-y-3">
      <div className="space-y-1.5">
        <label htmlFor="session-task" className="text-sm font-medium">
          Task
        </label>
        <select
          id="session-task"
          name="task_id"
          className="border-input bg-background h-8 w-full rounded-lg border px-2.5 text-sm"
          aria-describedby={state.error ? "session-error" : undefined}
          aria-invalid={state.error ? true : undefined}
        >
          {readyTasks.map((task) => (
            <option key={task.id} value={task.id}>
              {task.title}
            </option>
          ))}
        </select>
      </div>

      <div className="space-y-1.5">
        <label htmlFor="session-agent" className="text-sm font-medium">
          Agent
        </label>
        {/* The value is the *version* id, not the agent id. A session pins the
            settings it ran under, so that editing the profile afterwards
            cannot rewrite what a finished session claims to have done. */}
        <select
          id="session-agent"
          name="agent_version_id"
          className="border-input bg-background h-8 w-full rounded-lg border px-2.5 text-sm"
        >
          {runnable.map((agent) => (
            <option key={agent.id} value={agent.current_version_id}>
              {agent.name}
            </option>
          ))}
        </select>
      </div>

      <div className="space-y-1.5">
        <label htmlFor="session-base" className="text-sm font-medium">
          Base branch <span className="text-muted-foreground font-normal">(optional)</span>
        </label>
        <Input id="session-base" name="base_branch" placeholder="the repository's default" />
        <p className="text-muted-foreground text-xs">
          Left empty, the default branch is resolved when the branch is cut rather than now — it can
          change in between.
        </p>
      </div>

      {state.error ? (
        <p id="session-error" role="alert" className="text-destructive text-sm">
          {state.error}
        </p>
      ) : null}

      <SubmitButton />
    </form>
  );
}

function SubmitButton() {
  const { pending } = useFormStatus();
  return (
    <Button type="submit" disabled={pending}>
      {pending ? "Starting session…" : "Start session"}
    </Button>
  );
}
