import Link from "next/link";
// Clerk Core 3 replaced <SignedIn> / <SignedOut> with <Show when="...">.
import { Show } from "@clerk/nextjs";

import { Button } from "@/components/ui/button";

/** Public landing route. */
export default function Home() {
  return (
    <main className="flex min-h-screen flex-col items-center justify-center gap-6 p-6">
      <div className="space-y-2 text-center">
        <h1 className="text-3xl font-semibold tracking-tight">weave</h1>
        <p className="text-muted-foreground max-w-sm text-sm">
          The shared control room for human and AI work.
        </p>
      </div>

      {/*
        These primitives are Base UI based, so composition uses `render`
        rather than Radix's `asChild`.
      */}
      <Show when="signed-out">
        <Button render={<Link href="/sign-in">Sign in with GitHub</Link>} />
      </Show>

      <Show when="signed-in">
        <Button render={<Link href="/dashboard">Go to dashboard</Link>} />
      </Show>
    </main>
  );
}
