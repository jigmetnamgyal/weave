import type { NextConfig } from "next";

// Environment comes from the single .env at the repository root, shared with
// the Go API. Next.js only reads .env files from its own directory, so the
// task runner links apps/web/.env to it — see the `.env` target in the
// Makefile. Loading it from here instead does not work: next.config.ts runs
// too late to reach the edge runtime that proxy.ts executes in, or the
// NEXT_PUBLIC_ inlining the client bundle depends on.

const nextConfig: NextConfig = {/* config options here */};

export default nextConfig;
