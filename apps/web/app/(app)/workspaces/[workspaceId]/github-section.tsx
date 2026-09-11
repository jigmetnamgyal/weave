import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { ConnectGitHubButton } from "./connect-github-button";
import { SyncRepositoriesButton } from "./sync-repositories-button";
import {
  fetchInstallations,
  fetchRepositories,
  type Installation,
  type Repository,
  type Workspace,
} from "@/lib/api";

/**
 * The GitHub section of a workspace page.
 *
 * Rendered per installation rather than as one flat repository list. A
 * workspace may connect several accounts — a personal one and an organisation
 * is the common case — and flattening them hides which connection is the one
 * that is failing when one of them is.
 */
export async function GitHubSection({ workspace }: { workspace: Workspace }) {
  const canManage = workspace.permissions.includes("repository:manage");

  const [installations, repositories] = await Promise.all([
    fetchInstallations(workspace.id),
    fetchRepositories(workspace.id),
  ]);

  if (!installations.ok) {
    return (
      <Card>
        <CardHeader>
          <CardTitle className="text-base">GitHub</CardTitle>
          <CardDescription>{installations.message}</CardDescription>
        </CardHeader>
      </Card>
    );
  }

  const connected = installations.data;
  const all = repositories.ok ? repositories.data : [];

  if (connected.length === 0) {
    return (
      <Card>
        <CardHeader>
          <CardTitle className="text-base">GitHub</CardTitle>
          <CardDescription>
            No GitHub account is connected, so this workspace has no repositories yet.
          </CardDescription>
        </CardHeader>
        <CardContent>
          {canManage ? (
            <ConnectGitHubButton workspaceId={workspace.id} label="Connect GitHub" />
          ) : (
            <p className="text-muted-foreground text-sm">
              Your role cannot connect GitHub. Ask an owner or admin.
            </p>
          )}
        </CardContent>
      </Card>
    );
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">GitHub</CardTitle>
        <CardDescription>
          {connected.length === 1
            ? "One connected account."
            : `${connected.length} connected accounts.`}
        </CardDescription>
      </CardHeader>

      <CardContent className="space-y-6">
        {connected.map((installation) => (
          <InstallationBlock
            key={installation.id}
            workspaceId={workspace.id}
            installation={installation}
            repositories={all.filter((r) => r.installation_id === installation.id)}
            canManage={canManage}
          />
        ))}

        {canManage ? (
          <ConnectGitHubButton
            workspaceId={workspace.id}
            label="Connect another account, or change access"
            variant="secondary"
          />
        ) : null}
      </CardContent>
    </Card>
  );
}

function InstallationBlock({
  workspaceId,
  installation,
  repositories,
  canManage,
}: {
  workspaceId: string;
  installation: Installation;
  repositories: Repository[];
  canManage: boolean;
}) {
  const granted = repositories.filter((r) => r.granted);
  const withdrawn = repositories.filter((r) => !r.granted);

  return (
    <div className="space-y-3 border-l-2 border-border pl-4">
      <div>
        <p className="text-foreground text-sm font-medium">
          {installation.account_login}
          <span className="text-muted-foreground font-normal">
            {" "}
            · {installation.account_type === "Organization" ? "organisation" : "personal"}
          </span>
        </p>
        <p className="text-muted-foreground text-xs">
          {installation.suspended ? (
            <span className="text-destructive">
              Suspended on GitHub, so it grants nothing until it is restored.
            </span>
          ) : installation.repository_selection === "all" ? (
            `All repositories · ${granted.length} here`
          ) : (
            `Selected repositories · ${granted.length} here`
          )}
        </p>
      </div>

      {granted.length > 0 ? (
        <ul className="grid gap-1">
          {granted.map((repository) => (
            <li key={repository.id} className="flex items-center gap-2">
              <span className="font-mono text-xs">{repository.full_name}</span>
              {repository.private ? (
                <span className="text-muted-foreground text-xs">private</span>
              ) : null}
            </li>
          ))}
        </ul>
      ) : (
        <p className="text-muted-foreground text-sm">
          No repositories from this account have arrived yet.
        </p>
      )}

      {/*
        Withdrawn repositories are shown rather than dropped. Someone who used
        one yesterday should be told access was removed, not left wondering.
      */}
      {withdrawn.length > 0 ? (
        <div className="space-y-1">
          <p className="text-muted-foreground text-xs">No longer shared:</p>
          <ul className="grid gap-1">
            {withdrawn.map((repository) => (
              <li key={repository.id} className="text-muted-foreground font-mono text-xs">
                {repository.full_name}
              </li>
            ))}
          </ul>
        </div>
      ) : null}

      {canManage ? (
        <SyncRepositoriesButton
          workspaceId={workspaceId}
          installationId={installation.id}
          label={granted.length === 0 ? "Sync repositories from GitHub" : "Sync now"}
        />
      ) : null}
    </div>
  );
}
