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

  // The retry identity belongs to the component, not the hidden DOM input:
  // the latter disappears at the limit. Changed input is a new intent, while
  // resubmitting the same trimmed hostname retries the old request.
  const retry = useRef<{ hostname: string; key: string } | null>(null);
  // Only retain submitted, unconfirmed requests, not every intermediate
  // keystroke. Editing away and back must not destroy a lost-response replay.
  const unconfirmed = useRef(new Map<string, string>());
  const keyInput = useRef<HTMLInputElement>(null);
  const hostnameInput = useRef<HTMLInputElement>(null);

  function syncKey(hostname: string) {
    const identity = hostname.trim();
    if (!retry.current || retry.current.hostname !== identity) {
      retry.current = {
        hostname: identity,
        key: unconfirmed.current.get(identity) ?? crypto.randomUUID(),
      };
    }
    if (keyInput.current) keyInput.current.value = retry.current.key;
  }

  // Randomness stays out of render. Rotate on each successful action result,
  // even if its hostname is the same as an earlier successful submission.
  useEffect(() => {
    if (state.error && state.hostname && retry.current?.hostname === state.hostname.trim()) {
      unconfirmed.current.set(retry.current.hostname, retry.current.key);
    }
    if (state.added && retry.current) {
      // Confirming B must not discard an uncertain request for A.
      unconfirmed.current.delete(retry.current.hostname);
      retry.current = null;
    }
  }, [state]);
  useEffect(() => {
    if (hostnameInput.current) syncKey(hostnameInput.current.value);
  }, [state, atLimit]);

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
          key={state.error ? `error:${state.hostname ?? ""}` : `success:${state.added ?? ""}`}
          ref={hostnameInput}
          onChange={(event) => syncKey(event.currentTarget.value)}
          disabled={pending}
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
