"use client";

import { useRouter } from "next/navigation";
import { useState, useTransition } from "react";

import { Button } from "@/components/ui/button";
import { acceptInvitationAction } from "@/app/actions/invitations";

/** Accepts an invitation and moves the user into the workspace. */
export function AcceptInvitationButton({
  token,
  workspaceName,
}: {
  token: string;
  workspaceName: string;
}) {
  const router = useRouter();
  const [error, setError] = useState<string>();
  const [pending, startTransition] = useTransition();

  function accept() {
    setError(undefined);
    startTransition(async () => {
      const result = await acceptInvitationAction(token);
      if (result.error) {
        setError(result.error);
        return;
      }
      router.push(`/workspaces/${result.workspaceId}`);
    });
  }

  return (
    <div className="space-y-2">
      <Button onClick={accept} disabled={pending} className="w-full">
        {pending ? "Joining…" : `Join ${workspaceName}`}
      </Button>
      {error ? (
        <p role="alert" className="text-destructive text-sm">
          {error}
        </p>
      ) : null}
    </div>
  );
}
