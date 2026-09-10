// Next.js 16 renamed the `middleware` file convention to `proxy`. Clerk's
// helper is still called clerkMiddleware; only the file name changed.
import { clerkMiddleware } from "@clerk/nextjs/server";
import { NextResponse } from "next/server";

/** Header carrying the request path down to Server Components. */
export const PATHNAME_HEADER = "x-pathname";

/**
 * Makes the Clerk session available to every request, and forwards the request
 * path so layouts can tell which route is rendering.
 *
 * It deliberately does no route protection. Clerk deprecated
 * `createRouteMatcher` for exactly the reason it warns about: path matching
 * here can diverge from how Next.js actually resolves a route, which leaves
 * protected resources reachable while the pattern list looks correct.
 *
 * Protection instead lives with the resource it protects — see the layout in
 * app/(app), which every authenticated route renders inside.
 */
export default clerkMiddleware(async (_auth, request) => {
  // A layout receives params only for its own segment, so the root layout
  // cannot see a nested route's workspace id. Passing the path as a header is
  // how the header component learns which workspace is in scope.
  const requestHeaders = new Headers(request.headers);
  requestHeaders.set(PATHNAME_HEADER, request.nextUrl.pathname);

  return NextResponse.next({ request: { headers: requestHeaders } });
});

export const config = {
  matcher: [
    // Everything except Next.js internals and static files, unless a search
    // param is present — a static-looking path can still carry auth state.
    "/((?!_next|[^?]*\\.(?:html?|css|js(?!on)|jpe?g|webp|png|gif|svg|ttf|woff2?|ico|csv|docx?|xlsx?|zip|webmanifest)).*)",
    // Always run for API routes.
    "/(api|trpc)(.*)",
    // Clerk's auto-proxy path. Without it the handshake and token refresh
    // requests bypass this proxy and sessions fail to establish.
    "/__clerk/:path*",
  ],
};
