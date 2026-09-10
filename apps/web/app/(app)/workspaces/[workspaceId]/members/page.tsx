import { notFound } from "next/navigation";

import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { InviteMemberForm } from "@/components/invite-member-form";
import { RevokeInvitationButton } from "@/components/revoke-invitation-button";
import { fetchInvitations, fetchMembers, fetchWorkspaces, type Role } from "@/lib/api";

/** Roles the caller may grant, mirroring Role.CanGrant on the server. */
function grantableRoles(role: Role): Role[] {
  // Owner can grant anything; everyone else can grant at or below admin. The
  // server is authoritative — this only decides which options to render.
  return role === "owner"
    ? ["owner", "admin", "developer", "viewer"]
    : ["admin", "developer", "viewer"];
}

/** Members and pending invitations for a workspace. */
export default async function MembersPage({
  params,
}: PageProps<"/workspaces/[workspaceId]/members">) {
  const { workspaceId } = await params;

  const workspaces = await fetchWorkspaces();
  if (!workspaces.ok) throw new Error(workspaces.message);

  const workspace = workspaces.data.find((candidate) => candidate.id === workspaceId);
  if (!workspace) notFound();

  const canInvite = workspace.permissions.includes("member:invite");

  const [members, invitations] = await Promise.all([
    fetchMembers(workspaceId),
    canInvite ? fetchInvitations(workspaceId) : Promise.resolve(null),
  ]);

  const pending =
    invitations?.ok === true
      ? invitations.data.filter((invitation) => invitation.status === "pending")
      : [];

  return (
    <main className="mx-auto w-full max-w-3xl flex-1 space-y-6 p-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Members</h1>
        <p className="text-muted-foreground text-sm">{workspace.name}</p>
      </div>

      {canInvite ? (
        <Card>
          <CardHeader>
            <CardTitle className="text-base">Invite a teammate</CardTitle>
            <CardDescription>
              Weave does not send the email. Create the link and share it however you like.
            </CardDescription>
          </CardHeader>
          <CardContent>
            <InviteMemberForm
              workspaceId={workspaceId}
              grantableRoles={grantableRoles(workspace.role)}
            />
          </CardContent>
        </Card>
      ) : null}

      <Card>
        <CardHeader>
          <CardTitle className="text-base">
            {members.ok
              ? `${members.data.length} member${members.data.length === 1 ? "" : "s"}`
              : "Members"}
          </CardTitle>
        </CardHeader>
        <CardContent>
          {members.ok ? (
            <ul className="divide-border divide-y">
              {members.data.map((member) => (
                <li key={member.user_id} className="flex items-center justify-between py-2.5">
                  <div>
                    <p className="text-sm">{member.display_name || member.email}</p>
                    {member.display_name ? (
                      <p className="text-muted-foreground text-xs">{member.email}</p>
                    ) : null}
                  </div>
                  <span className="border-border text-muted-foreground rounded-full border px-2 py-0.5 text-[11px]">
                    {member.role}
                  </span>
                </li>
              ))}
            </ul>
          ) : (
            <p role="alert" className="text-destructive text-sm">
              {members.message}
            </p>
          )}
        </CardContent>
      </Card>

      {canInvite && pending.length > 0 ? (
        <Card>
          <CardHeader>
            <CardTitle className="text-base">Pending invitations</CardTitle>
            <CardDescription>Invitations expire seven days after they are created.</CardDescription>
          </CardHeader>
          <CardContent>
            <ul className="divide-border divide-y">
              {pending.map((invitation) => (
                <li key={invitation.id} className="flex items-center justify-between py-2.5">
                  <div>
                    <p className="text-sm">{invitation.email}</p>
                    <p className="text-muted-foreground text-xs">
                      {invitation.role} · expires{" "}
                      {new Date(invitation.expires_at).toLocaleDateString()}
                    </p>
                  </div>
                  <RevokeInvitationButton
                    workspaceId={workspaceId}
                    invitationId={invitation.id}
                    email={invitation.email}
                  />
                </li>
              ))}
            </ul>
          </CardContent>
        </Card>
      ) : null}
    </main>
  );
}
