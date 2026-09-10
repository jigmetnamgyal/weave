"use client";

import { useState, useTransition } from "react";

import { Button } from "@/components/ui/button";
import { revokeInvitationAction } from "@/app/actions/invitations";

/**
 * Withdraws a pending invitation.
 *
 * Revoking is not reversible — a new invitation carries a new token — so the
 * control asks once before acting.
 */
export function RevokeInvitationButton({
  workspaceId,
  invitationId,
  email,
}: {
  workspaceId: string;
  invitationId: string;
  email: string;
}) {
  const [confirming, setConfirming] = useState(false);
  const [error, setError] = useState<string>();
  const [pending, startTransition] = useTransition();

  function revoke() {
    startTransition(async () => {
      const result = await revokeInvitationAction(workspaceId, invitationId);
      if (result.error) setError(result.error);
      setConfirming(false);
    });
  }

  if (error) {
    return (
      <span role="alert" className="text-destructive text-xs">
        {error}
      </span>
    );
  }

  if (!confirming) {
    return (
      <Button
        variant="ghost"
        size="sm"
        onClick={() => setConfirming(true)}
        aria-label={`Revoke the invitation for ${email}`}
      >
        Revoke
      </Button>
    );
  }

  return (
    <span className="flex items-center gap-1.5">
      <span className="text-muted-foreground text-xs">Revoke?</span>
      <Button variant="destructive" size="sm" onClick={revoke} disabled={pending}>
        {pending ? "Revoking…" : "Yes"}
      </Button>
      <Button variant="ghost" size="sm" onClick={() => setConfirming(false)} disabled={pending}>
        No
      </Button>
    </span>
  );
}
