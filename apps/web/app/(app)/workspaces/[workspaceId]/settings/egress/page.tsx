import { notFound } from "next/navigation";

import { AddEgressHostForm } from "@/components/add-egress-host-form";
import { RemoveEgressHostButton } from "@/components/remove-egress-host-button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { fetchEgressHosts, fetchWorkspaces } from "@/lib/api";

/**
 * Added hosts: the destinations beyond GitHub and the package registries that
 * this workspace's sessions may reach (M5.4d.3b, ADR-017).
 *
 * Managing them needs `workspace:manage`, which the server checks on every
 * request; `permissions` only decides what to render. A member without it is
 * told why rather than shown an empty list, which would read as "none".
 */
export default async function EgressSettingsPage({
  params,
}: PageProps<"/workspaces/[workspaceId]/settings/egress">) {
  const { workspaceId } = await params;

  const workspaces = await fetchWorkspaces();
  if (!workspaces.ok) {
    return (
      <main className="mx-auto w-full max-w-3xl flex-1 space-y-6 p-6">
        <h1 className="text-2xl font-semibold tracking-tight">Added hosts</h1>
        <p role="alert" className="text-destructive text-sm">
          {workspaces.message}
        </p>
        <p className="text-muted-foreground text-xs">
          The workspace could not be loaded. Nothing has changed.
          {workspaces.requestId ? ` Reference: ${workspaces.requestId}.` : ""}
        </p>
      </main>
    );
  }
  const workspace = workspaces.data.find((candidate) => candidate.id === workspaceId);
  if (!workspace) notFound();

  const canManage = workspace.permissions.includes("workspace:manage");

  return (
    <main className="mx-auto w-full max-w-3xl flex-1 space-y-6 p-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Added hosts</h1>
        <p className="text-muted-foreground text-sm">{workspace.name}</p>
      </div>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">What adding a host does</CardTitle>
        </CardHeader>
        <CardContent className="text-muted-foreground space-y-2 text-sm">
          <p>
            Sessions can already reach GitHub and the public package registries. Adding a host lets
            them reach one more site, over HTTPS, through Weave&apos;s guarded proxy.
          </p>
          <p>
            <span className="text-foreground font-medium">
              An added host can receive your repository&apos;s data.
            </span>{" "}
            Add only sites you trust with it.
          </p>
          <p>
            Changes apply to sessions that start afterwards. Removing a host does not cut off a
            session that is already running; cancel that session to stop it at once.
          </p>
        </CardContent>
      </Card>

      {canManage ? <HostList workspaceId={workspaceId} /> : <NotPermitted role={workspace.role} />}
    </main>
  );
}

/** The list and the add form, for a member who may manage them. */
async function HostList({ workspaceId }: { workspaceId: string }) {
  const hosts = await fetchEgressHosts(workspaceId);

  // A failed fetch is not an empty list: saying "no hosts" when the list could
  // not be read would hide hosts that are in force.
  if (!hosts.ok) {
    return (
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Hosts</CardTitle>
        </CardHeader>
        <CardContent className="space-y-1">
          <p role="alert" className="text-destructive text-sm">
            {hosts.message}
          </p>
          <p className="text-muted-foreground text-xs">
            The list could not be loaded, so any added hosts are not shown here. Nothing has
            changed.
            {hosts.requestId ? ` Reference: ${hosts.requestId}.` : ""}
          </p>
        </CardContent>
      </Card>
    );
  }

  const { items, limit } = hosts.data;
  return (
    <>
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Add a host</CardTitle>
          <CardDescription>
            {items.length} of {limit} used.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <AddEgressHostForm workspaceId={workspaceId} atLimit={items.length >= limit} />
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">Hosts</CardTitle>
        </CardHeader>
        <CardContent>
          {items.length === 0 ? (
            <p className="text-muted-foreground text-sm">
              No hosts added. Sessions reach only GitHub and the public package registries.
            </p>
          ) : (
            <ul className="divide-border divide-y">
              {items.map((host) => (
                <li key={host.id} className="flex items-start justify-between gap-4 py-2.5">
                  <div className="min-w-0">
                    <p className="truncate font-mono text-sm">{host.hostname}</p>
                    <p className="text-muted-foreground text-xs">
                      Added{" "}
                      <time dateTime={host.created_at}>
                        {new Date(host.created_at).toLocaleDateString("en-GB", {
                          timeZone: "UTC",
                          day: "numeric",
                          month: "short",
                          year: "numeric",
                        })}{" "}
                        UTC
                      </time>
                      {" · Added by "}
                      <span className="break-all font-mono">{host.created_by}</span>
                    </p>
                  </div>
                  <RemoveEgressHostButton
                    workspaceId={workspaceId}
                    egressHostId={host.id}
                    hostname={host.hostname}
                  />
                </li>
              ))}
            </ul>
          )}
        </CardContent>
      </Card>
    </>
  );
}

/** Shown in place of the list to a member who cannot manage it. */
function NotPermitted({ role }: { role: string }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">Hosts</CardTitle>
      </CardHeader>
      <CardContent>
        <p className="text-muted-foreground text-sm">
          Your role is <span className="text-foreground">{role}</span>. Only owners and admins can
          view and change added hosts, because they decide where a session&apos;s code can send
          data.
        </p>
      </CardContent>
    </Card>
  );
}
