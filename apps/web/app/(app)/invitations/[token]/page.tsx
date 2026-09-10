import { AlertTriangle } from "lucide-react";

import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { AcceptInvitationButton } from "@/components/accept-invitation-button";
import { previewInvitation } from "@/lib/api";

/**
 * The page an invitation link opens.
 *
 * It sits inside the (app) route group, so an unauthenticated visitor is sent
 * to sign in and returned here afterwards — the token survives in the URL.
 *
 * The token is in the path because that is what a shareable link is. It never
 * reaches the API that way: the client sends it in a request body, keeping it
 * out of API access logs and Referer headers.
 */
export default async function InvitationPage({ params }: PageProps<"/invitations/[token]">) {
  const { token } = await params;
  const preview = await previewInvitation(token);

  if (!preview.ok) {
    return (
      <main className="flex flex-1 items-center justify-center p-6">
        <Card className="w-full max-w-md">
          <CardHeader>
            <CardTitle>This invitation cannot be used</CardTitle>
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
                <p>{preview.message}</p>
                <p className="text-muted-foreground text-xs">
                  Ask whoever invited you to send a new link.
                </p>
              </div>
            </div>
          </CardContent>
        </Card>
      </main>
    );
  }

  const invitation = preview.data;

  return (
    <main className="flex flex-1 items-center justify-center p-6">
      <Card className="w-full max-w-md">
        <CardHeader>
          <CardTitle>Join {invitation.workspace_name}</CardTitle>
          <CardDescription>
            {invitation.invited_by_display_name || invitation.invited_by_email} invited{" "}
            {invitation.email} to join as {invitation.role}.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <p className="text-muted-foreground text-sm">
            You must be signed in as {invitation.email} to accept. Expires{" "}
            {new Date(invitation.expires_at).toLocaleDateString()}.
          </p>
          <AcceptInvitationButton token={token} workspaceName={invitation.workspace_name} />
        </CardContent>
      </Card>
    </main>
  );
}
