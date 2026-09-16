import Link from "next/link";
import { notFound } from "next/navigation";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { GitHubSection } from "./github-section";
import { fetchWorkspaces } from "@/lib/api";

/**
 * A single workspace.
 *
 * The workspace's landing page: what the caller may do here, which GitHub
 * accounts are connected, and the way in to tasks, agents and sessions.
 *
 * The workspace is resolved from the caller's own list rather than fetched by
 * identifier, which means an identifier for a workspace the caller does not
 * belong to renders the same 404 as one that does not exist — matching what
 * the API does, so the two layers cannot disagree.
 */
export default async function WorkspacePage({ params }: PageProps<"/workspaces/[workspaceId]">) {
  const { workspaceId } = await params;

  const result = await fetchWorkspaces();
  if (!result.ok) {
    throw new Error(result.message);
  }

  const workspace = result.data.find((candidate) => candidate.id === workspaceId);
  if (!workspace) {
    notFound();
  }

  return (
    <main className="mx-auto w-full max-w-3xl flex-1 space-y-6 p-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">{workspace.name}</h1>
        <p className="text-muted-foreground font-mono text-xs">{workspace.slug}</p>
      </div>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">Your access</CardTitle>
          <CardDescription>
            Your role is <span className="text-foreground">{workspace.role}</span>.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <ul className="grid gap-1.5 text-sm sm:grid-cols-2">
            {workspace.permissions.map((permission) => (
              <li key={permission} className="text-muted-foreground font-mono text-xs">
                {permission}
              </li>
            ))}
          </ul>
        </CardContent>
      </Card>

      <GitHubSection workspace={workspace} />

      <Card>
        <CardHeader>
          <CardTitle className="text-base">Members</CardTitle>
          <CardDescription>See who is here, and invite your team.</CardDescription>
        </CardHeader>
        <CardContent>
          <Button
            variant="outline"
            size="sm"
            nativeButton={false}
            render={<Link href={`/workspaces/${workspace.id}/members`}>Manage members</Link>}
          />
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">Work</CardTitle>
          <CardDescription>
            A task describes what needs doing, an agent describes what will attempt it, and a
            session is one running against the other.
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-wrap gap-2">
          <Button
            variant="outline"
            size="sm"
            nativeButton={false}
            render={<Link href={`/workspaces/${workspace.id}/tasks`}>Tasks</Link>}
          />
          <Button
            variant="outline"
            size="sm"
            nativeButton={false}
            render={<Link href={`/workspaces/${workspace.id}/agents`}>Agents</Link>}
          />
          <Button
            variant="outline"
            size="sm"
            nativeButton={false}
            render={<Link href={`/workspaces/${workspace.id}/sessions`}>Sessions</Link>}
          />
        </CardContent>
      </Card>
    </main>
  );
}
