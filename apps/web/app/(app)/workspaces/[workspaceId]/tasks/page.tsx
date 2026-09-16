import { notFound } from "next/navigation";

import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { CreateTaskForm } from "@/components/create-task-form";
import { TaskReadyToggle } from "@/components/task-ready-toggle";
import { fetchRepositories, fetchTasks, fetchWorkspaces } from "@/lib/api";

/** Tasks: what someone wants done, before anything runs. */
export default async function TasksPage({ params }: PageProps<"/workspaces/[workspaceId]/tasks">) {
  const { workspaceId } = await params;

  const workspaces = await fetchWorkspaces();
  if (!workspaces.ok) throw new Error(workspaces.message);

  const workspace = workspaces.data.find((candidate) => candidate.id === workspaceId);
  if (!workspace) notFound();

  // An affordance, not a control. The API enforces the same permission.
  const canCreate = workspace.permissions.includes("session:create");

  const [tasks, repositories] = await Promise.all([
    fetchTasks(workspaceId),
    canCreate ? fetchRepositories(workspaceId) : Promise.resolve(null),
  ]);

  // A failed fetch is not an empty list, and the two must not share a
  // sentence: "no repositories yet" is a claim about GitHub when the fact is
  // that our request failed.
  const granted = repositories?.ok === true ? repositories.data.filter((r) => r.granted) : [];
  const repositoriesError = repositories?.ok === false ? repositories.message : undefined;

  return (
    <main className="mx-auto w-full max-w-3xl flex-1 space-y-6 p-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Tasks</h1>
        <p className="text-muted-foreground text-sm">{workspace.name}</p>
      </div>

      {canCreate ? (
        <Card>
          <CardHeader>
            <CardTitle className="text-base">New task</CardTitle>
            <CardDescription>
              A task describes the work. It becomes runnable once it is ready and names a
              repository.
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-3">
            {repositoriesError ? (
              <p role="alert" className="text-destructive text-sm">
                Repositories could not be loaded, so none can be chosen: {repositoriesError}
              </p>
            ) : null}
            {!repositoriesError && granted.length === 0 ? (
              <p className="text-muted-foreground text-sm">
                No repository is connected yet, so a task can be written but not marked ready.
              </p>
            ) : null}
            <CreateTaskForm workspaceId={workspaceId} repositories={granted} />
          </CardContent>
        </Card>
      ) : null}

      <Card>
        <CardHeader>
          <CardTitle className="text-base">
            {tasks.ok ? `${tasks.data.length} task${tasks.data.length === 1 ? "" : "s"}` : "Tasks"}
          </CardTitle>
        </CardHeader>
        <CardContent>
          {!tasks.ok ? (
            <p role="alert" className="text-destructive text-sm">
              {tasks.message}
            </p>
          ) : tasks.data.length === 0 ? (
            <p className="text-muted-foreground text-sm">No tasks yet.</p>
          ) : (
            <ul className="divide-border divide-y">
              {tasks.data.map((task) => (
                <li key={task.id} className="space-y-1.5 py-3">
                  <div className="flex items-start justify-between gap-3">
                    <div className="min-w-0">
                      <p className="text-sm">{task.title}</p>
                      <p className="text-muted-foreground text-xs">
                        {task.status}
                        {task.repository_id ? "" : " · no repository"}
                      </p>
                    </div>
                    {canCreate && task.status !== "archived" ? (
                      <TaskReadyToggle
                        workspaceId={workspaceId}
                        taskId={task.id}
                        ready={task.status === "ready"}
                        hasRepository={Boolean(task.repository_id)}
                      />
                    ) : null}
                  </div>
                  {/* Rendered as text, never as markup. The body is untrusted
                      input; `whitespace-pre-wrap` keeps the indentation the
                      author typed without interpreting any of it. */}
                  {task.body ? (
                    <p className="text-muted-foreground bg-muted/40 rounded-lg p-2.5 font-mono text-xs whitespace-pre-wrap">
                      {task.body}
                    </p>
                  ) : null}
                </li>
              ))}
            </ul>
          )}
        </CardContent>
      </Card>
    </main>
  );
}
