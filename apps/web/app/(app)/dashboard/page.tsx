import { SignOutButton } from "@clerk/nextjs";
import { AlertTriangle } from "lucide-react";

import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { fetchCurrentUser } from "@/lib/api";

/**
 * The first authenticated route.
 *
 * A Server Component: it calls the control-plane API during render with the
 * session token, so the token stays on the server and the browser makes no
 * cross-origin request.
 *
 * It exists to prove the whole path end to end — Clerk session, Go API
 * verification, and the internal user record the API resolved. The real
 * application shell replaces it.
 */
export default async function DashboardPage() {
  const result = await fetchCurrentUser();

  return (
    <main className="flex flex-1 items-center justify-center p-6">
      <Card className="w-full max-w-md">
        <CardHeader>
          <CardTitle>Signed in</CardTitle>
          <CardDescription>
            {result.ok
              ? "Your session was verified by the Weave API."
              : "Your session is active, but the API did not answer."}
          </CardDescription>
        </CardHeader>

        <CardContent className="space-y-4">
          {result.ok ? (
            <dl className="space-y-3 text-sm">
              <div className="space-y-1">
                <dt className="text-muted-foreground">User ID</dt>
                <dd className="font-mono text-xs break-all">{result.data.id}</dd>
              </div>
              <div className="space-y-1">
                <dt className="text-muted-foreground">Email</dt>
                <dd>{result.data.email}</dd>
              </div>
              {result.data.display_name ? (
                <div className="space-y-1">
                  <dt className="text-muted-foreground">Name</dt>
                  <dd>{result.data.display_name}</dd>
                </div>
              ) : null}
            </dl>
          ) : (
            <div
              role="alert"
              className="flex gap-3 rounded-md border border-destructive/40 bg-destructive/10 p-3 text-sm"
            >
              <AlertTriangle
                className="mt-0.5 size-4 shrink-0 text-destructive"
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
          )}

          <SignOutButton>
            <Button variant="outline" className="w-full">
              Sign out
            </Button>
          </SignOutButton>
        </CardContent>
      </Card>
    </main>
  );
}
