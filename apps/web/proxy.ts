// Next.js 16 renamed the `middleware` file convention to `proxy`. Clerk's
// helper is still called clerkMiddleware; only the file name changed.
import { clerkMiddleware } from "@clerk/nextjs/server";

/**
 * Makes the Clerk session available to every request.
 *
 * It deliberately does no route protection. Clerk deprecated
 * `createRouteMatcher` for exactly the reason it warns about: path matching
 * here can diverge from how Next.js actually resolves a route, which leaves
 * protected resources reachable while the pattern list looks correct.
 *
 * Protection instead lives with the resource it protects — see the layout in
 * app/(app), which every authenticated route renders inside.
 */
export default clerkMiddleware();

export const config = {
  matcher: [
    // Everything except Next.js internals and static files, unless a search
    // param is present — a static-looking path can still carry auth state.
    "/((?!_next|[^?]*\\.(?:html?|css|js(?!on)|jpe?g|webp|png|gif|svg|ttf|woff2?|ico|csv|docx?|xlsx?|zip|webmanifest)).*)",
    // Always run for API routes.
    "/(api|trpc)(.*)",
  ],
};
