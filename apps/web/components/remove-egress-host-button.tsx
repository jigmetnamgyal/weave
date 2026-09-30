"use client";

import { useActionState, useEffect, useRef, useState } from "react";

import { Button } from "@/components/ui/button";
import { removeEgressHostAction, type EgressFormState } from "@/app/actions/egress";

const initialState: EgressFormState = {};

/**
 * Remove an added host, after one confirmation.
 *
 * The confirmation says what removal does and does not do: sessions already
 * running keep the list they started with, so it is not an immediate cut-off.
 * Bound into `useActionState` as a form action so the route re-renders from the
 * action response.
 */
export function RemoveEgressHostButton({
  workspaceId,
  egressHostId,
  hostname,
}: {
  workspaceId: string;
  egressHostId: string;
  hostname: string;
}) {
  const [confirming, setConfirming] = useState(false);
  const [state, formAction, pending] = useActionState(
    removeEgressHostAction.bind(null, workspaceId, egressHostId),
    initialState
  );

  // Kept for this host's removal: a retry after a failure reuses it.
  const keyInput = useRef<HTMLInputElement>(null);
  useEffect(() => {
    if (keyInput.current && keyInput.current.value === "") {
      keyInput.current.value = crypto.randomUUID();
    }
  }, [confirming]);

  if (!confirming) {
    return (
      <div className="text-right">
        <Button
          variant="ghost"
          size="sm"
          onClick={() => setConfirming(true)}
          aria-label={`Remove ${hostname}`}
        >
          Remove
        </Button>
        {state.error ? (
          <p role="alert" className="text-destructive mt-1 text-xs">
            {state.error}
          </p>
        ) : null}
      </div>
    );
  }

  return (
    <form action={formAction} className="max-w-xs space-y-1.5 text-right">
      <input ref={keyInput} type="hidden" name="idempotency_key" defaultValue="" />
      <p className="text-muted-foreground text-xs">
        Remove {hostname}? New sessions will not reach it. Sessions already running keep it until
        they end.
      </p>
      <span className="flex justify-end gap-1.5">
        <Button type="submit" variant="destructive" size="sm" disabled={pending}>
          {pending ? "Removing…" : "Remove"}
        </Button>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          onClick={() => setConfirming(false)}
          disabled={pending}
        >
          Keep
        </Button>
      </span>
      {state.error ? (
        <p role="alert" className="text-destructive text-xs">
          {state.error}
        </p>
      ) : null}
    </form>
  );
}
