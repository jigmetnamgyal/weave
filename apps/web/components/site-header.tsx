import Link from "next/link";
import { Show, SignInButton, SignUpButton, UserButton } from "@clerk/nextjs";
import { auth } from "@clerk/nextjs/server";
import { headers } from "next/headers";

import { Button } from "@/components/ui/button";
import { fetchWorkspaces, type Workspace } from "@/lib/api";
import { PATHNAME_HEADER } from "@/proxy";

/**
 * Top bar carrying the authentication controls and workspace context.
 *
 * `Show` replaces the `<SignedIn>` / `<SignedOut>` components removed in
 * Clerk Core 3.
 *
 * The 52px height matches the top bar in `context/ui-context.md`, so the real
 * application shell can grow from here rather than replacing it.
 */
export async function SiteHeader() {
  const current = await currentWorkspace();

  return (
    <header className="border-border flex h-[52px] shrink-0 items-center justify-between border-b px-5">
      <div className="flex items-center gap-3">
        <Link href="/" className="text-sm font-semibold tracking-tight">
          weave
        </Link>

        {current ? (
          <>
            <span className="text-muted-foreground" aria-hidden="true">
              /
            </span>
            <Link href={`/workspaces/${current.id}`} className="flex items-center gap-2 text-sm">
              {current.name}
              {/*
                The role is shown next to the workspace because permissions
                differ by workspace: the same person may be an owner in one and
                a viewer in another, and acting on the wrong assumption is how
                someone is surprised by a refusal.
              */}
              <span className="border-border text-muted-foreground rounded-full border px-2 py-0.5 text-[11px]">
                {current.role}
              </span>
            </Link>
          </>
        ) : null}
      </div>

      <nav className="flex items-center gap-2">
        <Show when="signed-out">
          <SignInButton>
            <Button variant="ghost" size="sm">
              Sign in
            </Button>
          </SignInButton>
          <SignUpButton>
            <Button size="sm">Sign up</Button>
          </SignUpButton>
        </Show>

        <Show when="signed-in">
          {/*
            nativeButton={false} because this renders an <a>, not a <button>.
            Base UI otherwise applies native button semantics to a link.
          */}
          <Button
            variant="ghost"
            size="sm"
            nativeButton={false}
            render={<Link href="/dashboard">Workspaces</Link>}
          />
          <UserButton />
        </Show>
      </nav>
    </header>
  );
}

/**
 * Resolves the workspace to display, if any.
 *
 * Returns nothing when signed out, when no workspace is in scope, or when the
 * API is unreachable: the header is chrome, and a failure to load it must not
 * take down the page it frames.
 */
async function currentWorkspace(): Promise<Workspace | undefined> {
  const pathname = (await headers()).get(PATHNAME_HEADER) ?? "";
  const match = /^\/workspaces\/([0-9a-fA-F-]{36})(?:\/|$)/.exec(pathname);
  if (!match) return undefined;

  const workspaceId = match[1];

  const { userId } = await auth();
  if (!userId) return undefined;

  const result = await fetchWorkspaces();
  if (!result.ok) return undefined;

  return result.data.find((workspace) => workspace.id === workspaceId);
}
