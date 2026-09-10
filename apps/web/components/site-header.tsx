import Link from "next/link";
import { Show, SignInButton, SignUpButton, UserButton } from "@clerk/nextjs";

import { Button } from "@/components/ui/button";

/**
 * Top bar carrying the authentication controls.
 *
 * Signed out, it offers sign-in and sign-up. Signed in, it shows the Clerk
 * user button, which is how someone confirms at a glance which account they
 * are using and how they sign out.
 *
 * `Show` replaces the `<SignedIn>` / `<SignedOut>` components removed in
 * Clerk Core 3.
 *
 * The 52px height matches the top bar in `context/ui-context.md`, so the real
 * application shell can grow from here rather than replacing it.
 */
export function SiteHeader() {
  return (
    <header className="border-border flex h-[52px] shrink-0 items-center justify-between border-b px-5">
      <Link href="/" className="text-sm font-semibold tracking-tight">
        weave
      </Link>

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
            render={<Link href="/dashboard">Dashboard</Link>}
          />
          <UserButton />
        </Show>
      </nav>
    </header>
  );
}
