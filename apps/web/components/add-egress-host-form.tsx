"use client";

import { useActionState, useEffect, useRef } from "react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { addEgressHostAction, type EgressFormState } from "@/app/actions/egress";

const initialState: EgressFormState = {};

/**
 * Add a host a workspace's runners may reach.
 *
 * The API is the authority on what a valid hostname is; the input only hints.
 * On a refusal the typed value stays in the field (from the action's state),
 * and the idempotency key stays the same, so resubmitting is a retry of the
 * same request rather than a new one.
 */
export function AddEgressHostForm({
  workspaceId,
  atLimit,
}: {
  workspaceId: string;
  atLimit: boolean;
}) {
  const [state, formAction, pending] = useActionState(
    addEgressHostAction.bind(null, workspaceId),
    initialState
  );

  // One key per submission, written after mount rather than rendered: a value
  // generated during render would differ between the server and the browser.
  // A new key only after a success, so a failed attempt's retry reuses it.
  const keyInput = useRef<HTMLInputElement>(null);
  useEffect(() => {
    if (keyInput.current) keyInput.current.value = crypto.randomUUID();
  }, [state.added]);

  if (atLimit) {
    return (
      <p className="text-muted-foreground text-sm">
        This workspace has reached its limit of added hosts. Remove one to add another.
      </p>
    );
  }

  return (
    <form action={formAction} className="space-y-2">
      <input ref={keyInput} type="hidden" name="idempotency_key" defaultValue="" />
      <label htmlFor="egress-hostname" className="text-sm font-medium">
        Hostname
      </label>
      <div className="flex gap-2">
        <Input
          id="egress-hostname"
          name="hostname"
          // Keyed on the result so a success clears the field and a refusal
          // restores what was typed.
          key={state.added ?? state.hostname ?? "empty"}
          defaultValue={state.error ? state.hostname : ""}
          placeholder="docs.example.com"
          autoComplete="off"
          spellCheck={false}
          required
          maxLength={253}
          aria-describedby={state.error ? "egress-error" : "egress-hint"}
          aria-invalid={state.error ? true : undefined}
        />
        <Button type="submit" disabled={pending}>
          {pending ? "Adding…" : "Add"}
        </Button>
      </div>
      {state.error ? (
        <p id="egress-error" role="alert" className="text-destructive text-sm">
          {state.error}
        </p>
      ) : (
        <p id="egress-hint" className="text-muted-foreground text-xs">
          An exact hostname only — no https://, path, port or wildcard. Its subdomains are not
          included.
        </p>
      )}
      {state.added ? (
        <p role="status" className="text-muted-foreground text-sm">
          Added {state.added}. Sessions started from now on can reach it.
        </p>
      ) : null}
    </form>
  );
}
