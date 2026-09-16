"use client";

import { useActionState } from "react";
import { useFormStatus } from "react-dom";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { createAgentAction, type FormState } from "@/app/actions/sessions";
import type { Capability, Provider } from "@/lib/api";

const initialState: FormState = {};

/**
 * The providers the API accepts.
 *
 * `fake` is first, and deliberately: it is a deterministic adapter for
 * verifying orchestration before a paid provider is wired in, not a
 * placeholder to be removed later.
 */
const providers: { value: Provider; label: string }[] = [
  { value: "fake", label: "fake — deterministic, for verifying orchestration" },
  { value: "claude_code", label: "claude_code" },
  { value: "codex", label: "codex" },
];

/**
 * What a provider may support.
 *
 * A closed set, which the database also enforces. Free text here would make an
 * unrecognised capability a typo that silently disables a feature rather than
 * a rejected write.
 */
const capabilities: Capability[] = [
  "pause",
  "resume",
  "cancel",
  "send_instruction",
  "structured_tool_calls",
  "token_accounting",
];

/**
 * Define an agent profile.
 *
 * Rendered only for callers holding `workspace:manage`. A version carries the
 * tool policy — what an agent may do inside a customer's repository — so
 * defining one is configuration, and a developer may run agents without being
 * able to redefine what they are allowed to do.
 */
export function CreateAgentForm({ workspaceId }: { workspaceId: string }) {
  const [state, formAction] = useActionState(
    createAgentAction.bind(null, workspaceId),
    initialState
  );

  return (
    <form action={formAction} className="space-y-3">
      <div className="flex flex-col gap-3 sm:flex-row">
        <div className="flex-1 space-y-1.5">
          <label htmlFor="agent-name" className="text-sm font-medium">
            Name
          </label>
          <Input
            id="agent-name"
            name="name"
            placeholder="reviewer"
            required
            maxLength={80}
            aria-describedby={state.error ? "agent-error" : undefined}
            aria-invalid={state.error ? true : undefined}
          />
        </div>
        <div className="space-y-1.5">
          <label htmlFor="agent-provider" className="text-sm font-medium">
            Provider
          </label>
          <select
            id="agent-provider"
            name="provider"
            defaultValue="fake"
            className="border-input bg-background h-8 w-full rounded-lg border px-2.5 text-sm sm:w-72"
          >
            {providers.map((provider) => (
              <option key={provider.value} value={provider.value}>
                {provider.label}
              </option>
            ))}
          </select>
        </div>
      </div>

      <div className="space-y-1.5">
        <label htmlFor="agent-model" className="text-sm font-medium">
          Model
        </label>
        <Input id="agent-model" name="model" placeholder="deterministic-v1" required />
        <p className="text-muted-foreground text-xs">
          Not checked against a list: model names change faster than this code would, and the
          provider rejects one it does not have.
        </p>
      </div>

      <fieldset className="space-y-1.5">
        <legend className="text-sm font-medium">Capabilities</legend>
        <p className="text-muted-foreground text-xs">
          What this provider supports. Declared here and negotiated against the adapter in M5.
        </p>
        <div className="grid gap-1.5 sm:grid-cols-2">
          {capabilities.map((capability) => (
            <label key={capability} className="flex items-center gap-2 font-mono text-xs">
              <input
                type="checkbox"
                name="capabilities"
                value={capability}
                className="accent-primary size-4"
              />
              {capability}
            </label>
          ))}
        </div>
      </fieldset>

      {state.error ? (
        <p id="agent-error" role="alert" className="text-destructive text-sm">
          {state.error}
        </p>
      ) : null}

      <SubmitButton />
    </form>
  );
}

function SubmitButton() {
  const { pending } = useFormStatus();
  return (
    <Button type="submit" disabled={pending}>
      {pending ? "Creating agent…" : "Create agent"}
    </Button>
  );
}
