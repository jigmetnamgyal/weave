"use client";

import { useActionState } from "react";
import { useFormStatus } from "react-dom";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { createWorkspaceAction, type CreateWorkspaceState } from "@/app/actions/workspaces";

const initialState: CreateWorkspaceState = {};

/**
 * Creates a workspace.
 *
 * A client component only because it needs pending and error state; the work
 * itself happens in a Server Action, which forwards to the control-plane API.
 */
export function CreateWorkspaceForm() {
  const [state, formAction] = useActionState(createWorkspaceAction, initialState);

  return (
    <form action={formAction} className="space-y-3">
      <div className="space-y-1.5">
        <label htmlFor="workspace-name" className="text-sm font-medium">
          Workspace name
        </label>
        <Input
          id="workspace-name"
          name="name"
          placeholder="Acme Engineering"
          maxLength={80}
          required
          aria-describedby={state.error ? "workspace-name-error" : undefined}
          aria-invalid={state.error ? true : undefined}
        />
        {state.error ? (
          <p id="workspace-name-error" role="alert" className="text-destructive text-sm">
            {state.error}
          </p>
        ) : null}
      </div>
      <SubmitButton />
    </form>
  );
}

/**
 * Separate component because useFormStatus reads the status of the form it is
 * rendered inside, which means it cannot live in the component that owns the
 * form element.
 */
function SubmitButton() {
  const { pending } = useFormStatus();

  return (
    <Button type="submit" disabled={pending} className="w-full">
      {pending ? "Creating…" : "Create workspace"}
    </Button>
  );
}
