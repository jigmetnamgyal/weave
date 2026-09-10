import { SignIn } from "@clerk/nextjs";

/**
 * Sign-in route.
 *
 * The catch-all segment lets Clerk own its sub-routes (verification, factor
 * two, and so on) without each needing a file here.
 *
 * Which providers appear is Clerk dashboard configuration, not code: GitHub is
 * the only connection Weave enables, because repository work is the product
 * and every user will need a GitHub identity regardless.
 */
export default function SignInPage() {
  return (
    <main className="flex flex-1 items-center justify-center p-6">
      <SignIn />
    </main>
  );
}
