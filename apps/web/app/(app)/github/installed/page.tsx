import Link from "next/link";

import { completeGitHubInstall } from "@/lib/api";

/**
 * Where GitHub sends the browser after an installation.
 *
 * This page exists because GitHub redirects somewhere, not because there is
 * anything for a person to do here. It reads what GitHub appended to the URL
 * and asks the API to bind the installation.
 *
 * Two things it deliberately does not do. It does not decide which workspace
 * the installation belongs to — that was recorded before the redirect and is
 * recovered from the `state` value, because anything in this URL is under the
 * caller's control. And it does not trust `installation_id`; the API confirms
 * it with GitHub before writing anything.
 */
export default async function GitHubInstalledPage({
  searchParams,
}: {
  searchParams: Promise<Record<string, string | string[] | undefined>>;
}) {
  const params = await searchParams;
  const state = single(params.state);
  const installationId = Number(single(params.installation_id));
  const setupAction = single(params.setup_action);

  if (!state || !Number.isSafeInteger(installationId) || installationId <= 0) {
    return (
      <Outcome
        title="This link is incomplete"
        detail="GitHub did not send the values needed to finish connecting. Start again from your workspace."
      />
    );
  }

  const result = await completeGitHubInstall(state, installationId);

  if (!result.ok) {
    return (
      <Outcome
        title="That did not finish"
        detail={result.message}
        hint={
          setupAction === "update"
            ? "If you were changing which repositories are shared, the change is saved on GitHub and will appear here shortly."
            : "Installation links can only be used once, and expire after fifteen minutes. Start again from your workspace."
        }
        requestId={result.requestId}
      />
    );
  }

  return (
    <Outcome
      title={`Connected to ${result.data.account_login}`}
      detail={
        result.data.repository_selection === "all"
          ? "Every repository in this account is shared with the workspace."
          : "The repositories you selected are shared with the workspace."
      }
    />
  );
}

/** Query values arrive as string | string[]; take the first. */
function single(value: string | string[] | undefined): string {
  if (Array.isArray(value)) return value[0] ?? "";
  return value ?? "";
}

function Outcome({
  title,
  detail,
  hint,
  requestId,
}: {
  title: string;
  detail: string;
  hint?: string;
  requestId?: string;
}) {
  return (
    <main className="mx-auto flex min-h-svh max-w-md flex-col justify-center gap-4 px-6">
      <h1 className="text-xl font-medium text-foreground">{title}</h1>
      <p className="text-sm text-muted-foreground">{detail}</p>
      {hint ? <p className="text-sm text-muted-foreground">{hint}</p> : null}
      {requestId ? <p className="text-xs text-muted-foreground">Reference: {requestId}</p> : null}
      <Link href="/dashboard" className="text-sm text-primary underline underline-offset-4">
        Back to your workspaces
      </Link>
    </main>
  );
}
