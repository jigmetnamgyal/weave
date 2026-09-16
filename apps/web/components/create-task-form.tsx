"use client";

import { useActionState } from "react";
import { useFormStatus } from "react-dom";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { createTaskAction, type FormState } from "@/app/actions/sessions";
import type { Repository } from "@/lib/api";

const initialState: FormState = {};

/**
 * Describe a unit of work.
 *
 * Rendered only for callers holding `session:create`. That is an affordance,
 * not a control — the API enforces the same permission whichever way this is
 * reached.
 */
export function CreateTaskForm({
  workspaceId,
  repositories,
}: {
  workspaceId: string;
  /**
   * Only granted repositories are offered. A withdrawn one would be refused by
   * the API, and offering it would invite the refusal rather than prevent it.
   */
  repositories: Repository[];
}) {
  const [state, formAction] = useActionState(
    createTaskAction.bind(null, workspaceId),
    initialState
  );

  return (
    <form action={formAction} className="space-y-3">
      <div className="space-y-1.5">
        <label htmlFor="task-title" className="text-sm font-medium">
          Title
        </label>
        <Input
          id="task-title"
          name="title"
          placeholder="Add rate limiting to the public API"
          required
          maxLength={200}
          aria-describedby={state.error ? "task-error" : undefined}
          aria-invalid={state.error ? true : undefined}
        />
      </div>

      <div className="space-y-1.5">
        <label htmlFor="task-body" className="text-sm font-medium">
          What needs doing
        </label>
        <Textarea
          id="task-body"
          name="body"
          rows={5}
          maxLength={50000}
          placeholder="Describe the work. Indentation and code blocks are kept exactly as typed."
        />
        <p className="text-muted-foreground text-xs">
          Stored and returned exactly as written — nothing is trimmed or rewritten.
        </p>
      </div>

      <div className="space-y-1.5">
        <label htmlFor="task-repository" className="text-sm font-medium">
          Repository
        </label>
        <select
          id="task-repository"
          name="repository_id"
          defaultValue=""
          className="border-input bg-background h-8 w-full rounded-lg border px-2.5 text-sm"
        >
          <option value="">Not chosen yet</option>
          {repositories.map((repository) => (
            <option key={repository.id} value={repository.id}>
              {repository.owner}/{repository.name}
            </option>
          ))}
        </select>
      </div>

      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" name="ready" className="accent-primary size-4" />
        {/* A ready task is one a session can be created from, which is why it
            must name a repository — there would be nowhere to cut a branch. */}
        Ready to run — needs a repository
      </label>

      {state.error ? (
        <p id="task-error" role="alert" className="text-destructive text-sm">
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
      {pending ? "Creating task…" : "Create task"}
    </Button>
  );
}
