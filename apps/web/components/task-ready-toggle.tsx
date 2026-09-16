"use client";

import { useState, useTransition } from "react";

import { Button } from "@/components/ui/button";
import { setTaskReadyAction } from "@/app/actions/sessions";

/**
 * Move a task between draft and ready.
 *
 * The action is bound, not wrapped in a closure. A wrapped action leaves
 * `revalidatePath` invalidating the server cache while the router is never
 * told, which is how a working button came to look dead during M3.1.
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
  const [pending, startTransition] = useTransition();
  const [error, setError] = useState<string>();

  // A ready task must name a repository. Saying so beats letting the API
  // refuse it, which would be correct and less useful.
  if (!ready && !hasRepository) {
    return (
      <span className="text-muted-foreground shrink-0 text-xs">needs a repository to be ready</span>
    );
  }

  return (
    <div className="shrink-0 text-right">
      <Button
        size="sm"
        variant="outline"
        disabled={pending}
        onClick={() =>
          startTransition(async () => {
            const result = await setTaskReadyAction(workspaceId, taskId, !ready);
            setError(result.error);
          })
        }
      >
        {pending ? "Saving…" : ready ? "Back to draft" : "Mark ready"}
      </Button>
      {error ? (
        <p role="alert" className="text-destructive mt-1 text-xs">
          {error}
        </p>
      ) : null}
    </div>
  );
}
