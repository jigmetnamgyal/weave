"use client";

import { useActionState, useState } from "react";
import { useFormStatus } from "react-dom";
import { Check, Copy } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { inviteMemberAction, type InviteState } from "@/app/actions/invitations";
import type { Role } from "@/lib/api";

const initialState: InviteState = {};

/**
 * Invite someone to the workspace.
 *
 * Rendered only for callers holding `member:invite`. That is an affordance,
 * not a control — the API enforces the same permission.
 */
export function InviteMemberForm({
  workspaceId,
  grantableRoles,
}: {
  workspaceId: string;
  /**
   * Roles this caller may grant. An admin cannot invite an owner, so the
   * option is absent rather than present and rejected.
   */
  grantableRoles: Role[];
}) {
  const [state, formAction] = useActionState(
    inviteMemberAction.bind(null, workspaceId),
    initialState
  );

  return (
    <form action={formAction} className="space-y-3">
      <div className="flex flex-col gap-3 sm:flex-row">
        <div className="flex-1 space-y-1.5">
          <label htmlFor="invite-email" className="text-sm font-medium">
            Email address
          </label>
          <Input
            id="invite-email"
            name="email"
            type="email"
            placeholder="teammate@example.com"
            required
            aria-describedby={state.error ? "invite-error" : undefined}
            aria-invalid={state.error ? true : undefined}
          />
        </div>
        <div className="space-y-1.5">
          <label htmlFor="invite-role" className="text-sm font-medium">
            Role
          </label>
          <select
            id="invite-role"
            name="role"
            defaultValue={grantableRoles.includes("developer") ? "developer" : grantableRoles[0]}
            className="border-input bg-background h-8 w-full rounded-lg border px-2.5 text-sm sm:w-40"
          >
            {grantableRoles.map((role) => (
              <option key={role} value={role}>
                {role}
              </option>
            ))}
          </select>
        </div>
      </div>

      {state.error ? (
        <p id="invite-error" role="alert" className="text-destructive text-sm">
          {state.error}
        </p>
      ) : null}

      <SubmitButton />

      {state.link ? <InvitationLink link={state.link} email={state.email} /> : null}
    </form>
  );
}

function SubmitButton() {
  const { pending } = useFormStatus();
  return (
    <Button type="submit" disabled={pending}>
      {pending ? "Creating invite…" : "Create invite link"}
    </Button>
  );
}

/**
 * The one moment the token exists outside the recipient's link.
 *
 * Only its hash is stored, so this cannot be shown again — which the copy
 * says plainly, because a lost token means revoking and re-issuing.
 */
function InvitationLink({ link, email }: { link: string; email?: string }) {
  const [copied, setCopied] = useState(false);

  async function copy() {
    try {
      await navigator.clipboard.writeText(link);
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    } catch {
      // Clipboard access can be refused; the link is selectable either way.
    }
  }

  return (
    <div className="border-border bg-muted/40 space-y-2 rounded-md border p-3">
      <p className="text-sm">
        Invite link{email ? <> for {email}</> : null} — <strong>copy it now.</strong> It is not
        stored and cannot be shown again.
      </p>
      <div className="flex gap-2">
        <Input readOnly value={link} onFocus={(event) => event.currentTarget.select()} />
        <Button type="button" variant="outline" onClick={copy} aria-label="Copy invite link">
          {copied ? <Check /> : <Copy />}
        </Button>
      </div>
    </div>
  );
}
