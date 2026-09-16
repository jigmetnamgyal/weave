import { notFound } from "next/navigation";

import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { CreateAgentForm } from "@/components/create-agent-form";
import { fetchAgentVersions, fetchAgents, fetchWorkspaces } from "@/lib/api";

/** Agent profiles, and the versions a session can be pinned to. */
export default async function AgentsPage({
  params,
}: PageProps<"/workspaces/[workspaceId]/agents">) {
  const { workspaceId } = await params;

  const workspaces = await fetchWorkspaces();
  if (!workspaces.ok) throw new Error(workspaces.message);

  const workspace = workspaces.data.find((candidate) => candidate.id === workspaceId);
  if (!workspace) notFound();

  // `workspace:manage`, not `session:create`. A version carries the tool
  // policy — what an agent may do inside a customer's repository — so a
  // developer may run agents without being able to redefine what they may do.
  const canManage = workspace.permissions.includes("workspace:manage");

  const agents = await fetchAgents(workspaceId);

  // Versions are fetched per agent so the page can show what a session would
  // actually pin. A failed lookup leaves that agent's versions absent rather
  // than the page empty.
  //
  // One request per agent, which is worth naming rather than discovering: the
  // API has no endpoint that returns versions for a whole workspace, so this
  // is N+1 by construction. Fine for the handful of agents a workspace has
  // while this is a verification surface; if that stops being true, the fix is
  // an endpoint rather than a loop with a limit bolted on.
  const versions = agents.ok
    ? await Promise.all(
        agents.data.map(async (agent) => ({
          agentId: agent.id,
          result: await fetchAgentVersions(workspaceId, agent.id),
        }))
      )
    : [];

  return (
    <main className="mx-auto w-full max-w-3xl flex-1 space-y-6 p-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Agents</h1>
        <p className="text-muted-foreground text-sm">{workspace.name}</p>
      </div>

      {canManage ? (
        <Card>
          <CardHeader>
            <CardTitle className="text-base">New agent</CardTitle>
            <CardDescription>
              Editing a profile later writes a new version rather than changing this one, so a
              finished session keeps reading the settings it actually ran under.
            </CardDescription>
          </CardHeader>
          <CardContent>
            <CreateAgentForm workspaceId={workspaceId} />
          </CardContent>
        </Card>
      ) : null}

      <Card>
        <CardHeader>
          <CardTitle className="text-base">
            {agents.ok
              ? `${agents.data.length} agent${agents.data.length === 1 ? "" : "s"}`
              : "Agents"}
          </CardTitle>
        </CardHeader>
        <CardContent>
          {!agents.ok ? (
            <p role="alert" className="text-destructive text-sm">
              {agents.message}
            </p>
          ) : agents.data.length === 0 ? (
            <p className="text-muted-foreground text-sm">No agents yet.</p>
          ) : (
            <ul className="divide-border divide-y">
              {agents.data.map((agent) => {
                const found = versions.find((entry) => entry.agentId === agent.id)?.result;
                return (
                  <li key={agent.id} className="space-y-2 py-3">
                    <p className="text-sm">{agent.name}</p>
                    {found && !found.ok ? (
                      <p role="alert" className="text-destructive text-xs">
                        Versions could not be loaded: {found.message}
                      </p>
                    ) : null}
                    {found?.ok ? (
                      <ul className="space-y-1">
                        {found.data.map((version) => (
                          <li
                            key={version.id}
                            className="text-muted-foreground flex flex-wrap items-center gap-2 font-mono text-xs"
                          >
                            <span className="border-border rounded-full border px-2 py-0.5">
                              v{version.version}
                            </span>
                            <span>{version.provider}</span>
                            <span>{version.model}</span>
                            {version.id === agent.current_version_id ? (
                              <span className="text-foreground">· current</span>
                            ) : null}
                            {version.capabilities.length > 0 ? (
                              <span>· {version.capabilities.join(", ")}</span>
                            ) : null}
                          </li>
                        ))}
                      </ul>
                    ) : null}
                  </li>
                );
              })}
            </ul>
          )}
        </CardContent>
      </Card>
    </main>
  );
}
