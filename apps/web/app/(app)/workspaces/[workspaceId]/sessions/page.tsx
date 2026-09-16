import { notFound } from "next/navigation";

import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { StartSessionForm } from "@/components/start-session-form";
import { fetchAgents, fetchSessions, fetchTasks, fetchWorkspaces } from "@/lib/api";

/** Sessions: a run of an agent against a task. */
export default async function SessionsPage({
  params,
}: PageProps<"/workspaces/[workspaceId]/sessions">) {
  const { workspaceId } = await params;

  const workspaces = await fetchWorkspaces();
  if (!workspaces.ok) throw new Error(workspaces.message);

  const workspace = workspaces.data.find((candidate) => candidate.id === workspaceId);
  if (!workspace) notFound();

  const canCreate = workspace.permissions.includes("session:create");

  const [sessions, tasks, agents] = await Promise.all([
    fetchSessions(workspaceId),
    canCreate ? fetchTasks(workspaceId) : Promise.resolve(null),
    canCreate ? fetchAgents(workspaceId) : Promise.resolve(null),
  ]);

  const readyTasks = tasks?.ok === true ? tasks.data.filter((task) => task.status === "ready") : [];
  const availableAgents = agents?.ok === true ? agents.data : [];
  // Separate sentences for a failed request and an empty result, so the form's
  // "no task is ready yet" is never shown because a fetch failed.
  const inputsError =
    tasks?.ok === false ? tasks.message : agents?.ok === false ? agents.message : undefined;

  return (
    <main className="mx-auto w-full max-w-3xl flex-1 space-y-6 p-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Sessions</h1>
        <p className="text-muted-foreground text-sm">{workspace.name}</p>
      </div>

      {canCreate ? (
        <Card>
          <CardHeader>
            <CardTitle className="text-base">Start a session</CardTitle>
            <CardDescription>
              Nothing runs yet. A session is recorded as queued, with the branch it will use, and
              the workflow that acts on it arrives in M5.
            </CardDescription>
          </CardHeader>
          <CardContent>
            {inputsError ? (
              <p role="alert" className="text-destructive text-sm">
                Tasks or agents could not be loaded, so a session cannot be started: {inputsError}
              </p>
            ) : (
              <StartSessionForm
                workspaceId={workspaceId}
                readyTasks={readyTasks}
                agents={availableAgents}
              />
            )}
          </CardContent>
        </Card>
      ) : null}

      <Card>
        <CardHeader>
          <CardTitle className="text-base">
            {sessions.ok
              ? `${sessions.data.length} session${sessions.data.length === 1 ? "" : "s"}`
              : "Sessions"}
          </CardTitle>
        </CardHeader>
        <CardContent>
          {!sessions.ok ? (
            <p role="alert" className="text-destructive text-sm">
              {sessions.message}
            </p>
          ) : sessions.data.length === 0 ? (
            <p className="text-muted-foreground text-sm">No sessions yet.</p>
          ) : (
            <ul className="divide-border divide-y">
              {sessions.data.map((session) => (
                <li key={session.id} className="space-y-1.5 py-3">
                  <div className="flex flex-wrap items-center gap-2">
                    <span className="border-border rounded-full border px-2 py-0.5 text-[11px]">
                      {session.state}
                    </span>
                    <span className="text-muted-foreground font-mono text-xs">
                      v{session.version}
                    </span>
                    <span className="text-muted-foreground text-xs">
                      {new Date(session.created_at).toLocaleString()}
                    </span>
                  </div>
                  {/* Said plainly, because the name reads like a link and is
                      not one: the branch is created by the workflow in M5. */}
                  <p className="text-muted-foreground font-mono text-xs">
                    {session.branch_name}{" "}
                    <span className="font-sans">— not created yet; the workflow cuts it</span>
                  </p>
                  {/* Offered by the server from its transition table rather
                      than derived here, so there is only one statement of what
                      may follow a state. */}
                  {session.next_states.length > 0 ? (
                    <p className="text-muted-foreground text-xs">
                      can become: {session.next_states.join(", ")}
                    </p>
                  ) : (
                    <p className="text-muted-foreground text-xs">terminal — nothing follows</p>
                  )}
                </li>
              ))}
            </ul>
          )}
        </CardContent>
      </Card>
    </main>
  );
}
