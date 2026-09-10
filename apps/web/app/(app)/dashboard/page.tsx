import Link from "next/link";
import { AlertTriangle } from "lucide-react";

import {
  Card,
  CardContent,
  CardDescription,
  CardFooter,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { CreateWorkspaceForm } from "@/components/create-workspace-form";
import { fetchWorkspaces } from "@/lib/api";

/**
 * The authenticated landing page.
 *
 * A user with no workspace is asked to create one — a workspace is the scope
 * everything else hangs from, so there is nothing useful to show before one
 * exists. A user with workspaces sees them listed.
 */
export default async function DashboardPage() {
  const result = await fetchWorkspaces();

  if (!result.ok) {
    return (
      <main className="flex flex-1 items-center justify-center p-6">
        <Card className="w-full max-w-md">
          <CardHeader>
            <CardTitle>Could not load your workspaces</CardTitle>
          </CardHeader>
          <CardContent>
            <div
              role="alert"
              className="border-destructive/40 bg-destructive/10 flex gap-3 rounded-md border p-3 text-sm"
            >
              <AlertTriangle
                className="text-destructive mt-0.5 size-4 shrink-0"
                aria-hidden="true"
              />
              <div className="space-y-1">
                <p>{result.message}</p>
                <p className="text-muted-foreground text-xs">
                  Check that the API is running — <code>make health</code>.
                </p>
                {result.requestId ? (
                  <p className="text-muted-foreground font-mono text-xs break-all">
                    Request {result.requestId}
                  </p>
                ) : null}
              </div>
            </div>
          </CardContent>
        </Card>
      </main>
    );
  }

  const workspaces = result.data;

  if (workspaces.length === 0) {
    return (
      <main className="flex flex-1 items-center justify-center p-6">
        <Card className="w-full max-w-md">
          <CardHeader>
            <CardTitle>Create your first workspace</CardTitle>
            <CardDescription>
              A workspace is where your repositories, tasks and agent sessions live. You can invite
              your team to it later.
            </CardDescription>
          </CardHeader>
          <CardContent>
            <CreateWorkspaceForm />
          </CardContent>
        </Card>
      </main>
    );
  }

  return (
    <main className="mx-auto w-full max-w-3xl flex-1 space-y-6 p-6">
      <div className="flex items-end justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">Workspaces</h1>
          <p className="text-muted-foreground text-sm">
            {workspaces.length === 1 ? "One workspace" : `${workspaces.length} workspaces`}
          </p>
        </div>
      </div>

      <ul className="space-y-3">
        {workspaces.map((workspace) => (
          <li key={workspace.id}>
            <Card>
              <CardHeader>
                <CardTitle className="text-base">{workspace.name}</CardTitle>
                <CardDescription className="font-mono text-xs">
                  {workspace.slug} · you are {workspace.role}
                </CardDescription>
              </CardHeader>
              <CardFooter>
                <Button
                  variant="outline"
                  size="sm"
                  nativeButton={false}
                  render={<Link href={`/workspaces/${workspace.id}`}>Open</Link>}
                />
              </CardFooter>
            </Card>
          </li>
        ))}
      </ul>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">New workspace</CardTitle>
        </CardHeader>
        <CardContent>
          <CreateWorkspaceForm />
        </CardContent>
      </Card>
    </main>
  );
}
