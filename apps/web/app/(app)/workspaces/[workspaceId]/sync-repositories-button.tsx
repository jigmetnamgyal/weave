"use client";

import { useActionState } from "react";

import { Button } from "@/components/ui/button";
import { refreshRepositoriesAction, type SyncState } from "@/app/actions/github";

/**
 * Pull the current repository set from GitHub.
 *
 * Worth having a visible control rather than relying on webhooks and
 * reconciliation alone. Both of those are correct but invisible: when a
 * workspace shows a connection and no repositories, someone needs a way to say
 * "try again now" and see what happens.
 */
export function SyncRepositoriesButton({
  workspaceId,
  installationId,
  label,
}: {
  workspaceId: string;
  installationId: string;
  label: string;
}) {
  // Bound, not wrapped. A closure around the action would still run it, but
  // Next would not treat the result as an action response and the router would
  // never re-render — the sync would work and the page would not move.
  const [state, action, pending] = useActionState<SyncState, FormData>(
    refreshRepositoriesAction.bind(null, workspaceId, installationId),
    {}
  );

  return (
    <form action={action} className="space-y-2">
      <Button type="submit" variant="secondary" disabled={pending}>
        {pending ? "Asking GitHub…" : label}
      </Button>
      {state.error ? <p className="text-destructive text-sm">{state.error}</p> : null}
    </form>
  );
}
