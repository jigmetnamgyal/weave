import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { ConnectGitHubButton } from "./connect-github-button";
import { SyncRepositoriesButton } from "./sync-repositories-button";
import { fetchInstallations, fetchRepositories, type Workspace } from "@/lib/api";

/**
 * The GitHub section of a workspace page.
 *
 * Rendered on the server so the repository list reflects what the API will
 * actually authorize, rather than a cached view of it.
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
  if (connected.length === 0) {
    return (
      <Card>
        <CardHeader>
          <CardTitle className="text-base">GitHub</CardTitle>
          <CardDescription>
            No GitHub account is connected, so this workspace has no repositories yet.
          </CardDescription>
        </CardHeader>
        {canManage ? (
          <CardContent>
            <ConnectGitHubButton workspaceId={workspace.id} label="Connect GitHub" />
          </CardContent>
        ) : (
          <CardContent>
            <p className="text-muted-foreground text-sm">
              Your role cannot connect GitHub. Ask an owner or admin.
            </p>
          </CardContent>
        )}
      </Card>
    );
  }

  const granted = repositories.ok ? repositories.data.filter((r) => r.granted) : [];
  const withdrawn = repositories.ok ? repositories.data.filter((r) => !r.granted) : [];

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">GitHub</CardTitle>
        <CardDescription>
          {connected.map((installation) => (
            <span key={installation.id} className="block">
              {installation.account_login}
              {installation.suspended ? (
                <span className="text-destructive">
                  {" "}
                  — suspended on GitHub, so it grants nothing until it is restored
                </span>
              ) : installation.repository_selection === "all" ? (
                " — all repositories"
              ) : (
                " — selected repositories"
              )}
            </span>
          ))}
        </CardDescription>
      </CardHeader>

      <CardContent className="space-y-4">
        {granted.length > 0 ? (
          <ul className="grid gap-1.5 text-sm">
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
          <div className="space-y-2">
            <p className="text-muted-foreground text-sm">
              No repositories are shared with this workspace yet. If you selected some on GitHub,
              they may not have arrived here yet.
            </p>
            {canManage ? (
              <SyncRepositoriesButton
                workspaceId={workspace.id}
                installationId={connected[0].id}
                label="Sync repositories from GitHub"
              />
            ) : null}
          </div>
        )}

        {/*
          Withdrawn repositories are shown rather than dropped. Someone who
          used one yesterday should be told access was removed, not left to
          wonder where it went.
        */}
        {withdrawn.length > 0 ? (
          <div className="space-y-1.5">
            <p className="text-muted-foreground text-xs">No longer shared with this workspace:</p>
            <ul className="grid gap-1 text-sm">
              {withdrawn.map((repository) => (
                <li key={repository.id} className="text-muted-foreground font-mono text-xs">
                  {repository.full_name}
                </li>
              ))}
            </ul>
          </div>
        ) : null}

        {canManage ? (
          <div className="flex flex-wrap gap-2">
            <ConnectGitHubButton
              workspaceId={workspace.id}
              label="Change repository access on GitHub"
              variant="secondary"
            />
            {granted.length > 0 ? (
              <SyncRepositoriesButton
                workspaceId={workspace.id}
                installationId={connected[0].id}
                label="Sync now"
              />
            ) : null}
          </div>
        ) : null}
      </CardContent>
    </Card>
  );
}
