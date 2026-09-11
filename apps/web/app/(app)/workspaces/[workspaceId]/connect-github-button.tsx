"use client";

import { useActionState } from "react";

import { Button } from "@/components/ui/button";
import { connectGitHubAction, type ConnectState } from "@/app/actions/github";

/**
 * The button that starts a GitHub installation.
 *
 * A client component only so that a failure has somewhere to go. On success
 * the action redirects and nothing here renders again; on failure it returns a
 * message, which a plain server-action form could not surface.
 */
export function ConnectGitHubButton({
  workspaceId,
  label,
  variant,
}: {
  workspaceId: string;
  label: string;
  variant?: "default" | "secondary";
}) {
  const [state, action, pending] = useActionState<ConnectState, FormData>(
    connectGitHubAction.bind(null, workspaceId),
    {}
  );

  return (
    <form action={action} className="space-y-2">
      <Button type="submit" variant={variant} disabled={pending}>
        {pending ? "Opening GitHub…" : label}
      </Button>
      {state.error ? <p className="text-destructive text-sm">{state.error}</p> : null}
    </form>
  );
}
