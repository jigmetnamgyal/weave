import { SignUp } from "@clerk/nextjs";

/**
 * Sign-up route.
 *
 * Scaffolded by `clerk init`, then aligned with the sign-in route: the same
 * `flex-1` layout so both sit correctly inside the flex column that
 * app/layout.tsx establishes, rather than forcing their own full viewport
 * height and double-counting it.
 *
 * As with sign-in, which providers appear is Clerk dashboard configuration.
 */
export default function SignUpPage() {
  return (
    <main className="flex flex-1 items-center justify-center p-6">
      <SignUp />
    </main>
  );
}
