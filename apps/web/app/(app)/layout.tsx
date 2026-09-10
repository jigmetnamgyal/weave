import { auth } from "@clerk/nextjs/server";

/**
 * Layout for every authenticated route.
 *
 * The authentication check lives here rather than in path patterns in
 * proxy.ts, following Clerk's guidance that path matching can diverge from
 * how Next.js resolves routes. Because this runs for anything nested inside
 * the (app) route group, a page added to that group is protected by where it
 * sits in the tree — the same deny-by-default arrangement the API uses for
 * its /v1 subtree.
 *
 * An unauthenticated request is redirected to sign-in and never renders the
 * child.
 */
export default async function AppLayout({ children }: LayoutProps<"/">) {
  await auth.protect();

  return children;
}
