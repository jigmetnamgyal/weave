"use client";

import { useActionState } from "react";

import { Button } from "@/components/ui/button";
import { setTaskReadyAction, type FormState } from "@/app/actions/sessions";

const initialState: FormState = {};

/**
 * Move a task between draft and ready.
 *
 * Bound into `useActionState`, not called from a click handler. The first
 * version of this component did the latter — `startTransition(async () => …)`
 * around the action — under a comment claiming it was bound. It would have
 * updated the task and left the page showing the old status, which is the
 * M3.1 defect exactly: the sync worked and the page did not move. Next
 * re-renders the route only when the result arrives as an action response,
 * and a closure does not produce one.
 */
export function TaskReadyToggle({
  workspaceId,
  taskId,
  ready,
  hasRepository,
}: {
  workspaceId: string;
  taskId: string;
  ready: boolean;
  hasRepository: boolean;
}) {
  const [state, action, pending] = useActionState<FormState, FormData>(
    setTaskReadyAction.bind(null, workspaceId, taskId, !ready),
    initialState
  );

  // A ready task must name a repository. Saying so beats letting the API
  // refuse it, which would be correct and less useful.
  if (!ready && !hasRepository) {
    return (
      <span className="text-muted-foreground shrink-0 text-xs">needs a repository to be ready</span>
    );
  }

  return (
    <form action={action} className="shrink-0 text-right">
      <Button type="submit" size="sm" variant="outline" disabled={pending}>
        {pending ? "Saving…" : ready ? "Back to draft" : "Mark ready"}
      </Button>
      {state.error ? (
        // role="alert" because this appears after the action returns; without
        // a live region the failure is silent for anyone using a screen reader.
        <p role="alert" className="text-destructive mt-1 text-xs">
          {state.error}
        </p>
      ) : null}
    </form>
  );
}
