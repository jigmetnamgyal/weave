import Link from "next/link";
import { notFound } from "next/navigation";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import {
  fetchSession,
  fetchSessionParticipants,
  fetchSessionTransitions,
  fetchWorkspaces,
} from "@/lib/api";

/** Formats a timestamp in UTC, with the zone named. */
function utc(value: string): string {
  // Explicit, because this renders on the server: `toLocaleString()` with no
  // arguments would use the server's locale and zone and present them as the
  // reader's, with nothing saying which zone was meant.
  return new Date(value).toLocaleString("en-GB", { timeZone: "UTC", timeZoneName: "short" });
}

/**
 * One session: what it will run, who is in it, and every state it has been in.
 *
 * The participant and the history are the two side effects of creating a
 * session that the list cannot show, and seeing them is how a person confirms
 * that the six writes really did land together.
 */
export default async function SessionPage({
  params,
}: PageProps<"/workspaces/[workspaceId]/sessions/[sessionId]">) {
  const { workspaceId, sessionId } = await params;

  const workspaces = await fetchWorkspaces();
  if (!workspaces.ok) throw new Error(workspaces.message);

  const workspace = workspaces.data.find((candidate) => candidate.id === workspaceId);
  if (!workspace) notFound();

  const session = await fetchSession(workspaceId, sessionId);
  // 404 rather than an error page: a session in another workspace is absent,
  // not forbidden, which is the answer the API gives for the same reason.
  if (!session.ok && session.status === 404) notFound();

  const [participants, transitions] = session.ok
    ? await Promise.all([
        fetchSessionParticipants(workspaceId, sessionId),
        fetchSessionTransitions(workspaceId, sessionId),
      ])
    : [null, null];

  return (
    <main className="mx-auto w-full max-w-3xl flex-1 space-y-6 p-6">
      <div className="space-y-1">
        <Button
          variant="ghost"
          size="sm"
          nativeButton={false}
          render={<Link href={`/workspaces/${workspaceId}/sessions`}>← Sessions</Link>}
        />
        <h1 className="text-2xl font-semibold tracking-tight">Session</h1>
        <p className="text-muted-foreground font-mono text-xs">{sessionId}</p>
      </div>

      {!session.ok ? (
        <p role="alert" className="text-destructive text-sm">
          {session.message}
        </p>
      ) : (
        <>
          <Card>
            <CardHeader>
              <CardTitle className="text-base">State</CardTitle>
              <CardDescription>
                Nothing runs yet. The workflow that acts on this arrives in M5.
              </CardDescription>
            </CardHeader>
            <CardContent className="space-y-2 text-sm">
              <p>
                <span className="border-border rounded-full border px-2 py-0.5 text-[11px]">
                  {session.data.state}
                </span>{" "}
                <span className="text-muted-foreground font-mono text-xs">
                  v{session.data.version}
                </span>
              </p>
              <p className="text-muted-foreground font-mono text-xs">
                {session.data.branch_name}{" "}
                <span className="font-sans">— not created yet; the workflow cuts it</span>
              </p>
              <p className="text-muted-foreground text-xs">
                {session.data.next_states.length > 0
                  ? `can become: ${session.data.next_states.join(", ")}`
                  : "terminal — nothing follows"}
              </p>
              <p className="text-muted-foreground text-xs">
                created {utc(session.data.created_at)}
              </p>
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle className="text-base">Participants</CardTitle>
            </CardHeader>
            <CardContent>
              {participants && !participants.ok ? (
                <p role="alert" className="text-destructive text-sm">
                  {participants.message}
                </p>
              ) : participants?.ok && participants.data.length === 0 ? (
                <p className="text-muted-foreground text-sm">Nobody is in this session.</p>
              ) : (
                <ul className="divide-border divide-y">
                  {participants?.ok
                    ? participants.data.map((participant) => (
                        <li
                          key={participant.user_id}
                          className="flex items-center justify-between py-2"
                        >
                          <span className="font-mono text-xs">{participant.user_id}</span>
                          <span className="text-muted-foreground text-xs">
                            {participant.capacity}
                          </span>
                        </li>
                      ))
                    : null}
                </ul>
              )}
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle className="text-base">History</CardTitle>
              <CardDescription>
                Append-only. A correction is a new transition, never an edit — the database refuses
                UPDATE, DELETE and TRUNCATE on this table.
              </CardDescription>
            </CardHeader>
            <CardContent>
              {transitions && !transitions.ok ? (
                <p role="alert" className="text-destructive text-sm">
                  {transitions.message}
                </p>
              ) : transitions?.ok && transitions.data.length === 0 ? (
                <p className="text-muted-foreground text-sm">No transitions recorded.</p>
              ) : (
                <ul className="divide-border divide-y">
                  {transitions?.ok
                    ? transitions.data.map((transition) => (
                        <li key={transition.id} className="space-y-1 py-2">
                          <p className="text-sm">
                            {/* Absent on the first transition: a session comes
                                into existence already in a state, so recording
                                a move it never made would be a fiction. */}
                            {transition.previous_state ? (
                              <>
                                <span className="text-muted-foreground">
                                  {transition.previous_state}
                                </span>{" "}
                                →{" "}
                              </>
                            ) : (
                              <span className="text-muted-foreground">created in </span>
                            )}
                            {transition.next_state}
                          </p>
                          <p className="text-muted-foreground text-xs">
                            observed v{transition.observed_version} · {utc(transition.created_at)}
                            {transition.reason ? ` · ${transition.reason}` : ""}
                            {/* Absent when the system moved the session: a
                                timeout is not attributable to a person. */}
                            {transition.actor_user_id ? "" : " · by the system"}
                          </p>
                        </li>
                      ))
                    : null}
                </ul>
              )}
            </CardContent>
          </Card>
        </>
      )}
    </main>
  );
}
