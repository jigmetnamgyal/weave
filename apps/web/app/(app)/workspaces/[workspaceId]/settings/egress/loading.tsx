/** A skeleton in the page's own shape, so it does not jump when content arrives. */
export default function Loading() {
  return (
    <main
      className="mx-auto w-full max-w-3xl flex-1 space-y-6 p-6"
      aria-busy="true"
      aria-live="polite"
    >
      <span className="sr-only">Loading added hosts…</span>
      <div className="space-y-2">
        <div className="bg-muted h-7 w-40 animate-pulse rounded motion-reduce:animate-none" />
        <div className="bg-muted h-4 w-28 animate-pulse rounded motion-reduce:animate-none" />
      </div>
      {[112, 96, 144].map((height) => (
        <div
          key={height}
          className="bg-muted animate-pulse rounded-xl motion-reduce:animate-none"
          style={{ height }}
        />
      ))}
    </main>
  );
}
