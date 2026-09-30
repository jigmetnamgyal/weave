//go:build vercel_live

package vercelsandbox

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/application"
)

// Helpers for live acceptance tests in other packages — the registry proxy's
// (M5.4c) — which need a real sandbox with a given policy and a way to run a
// command in it. Compiled only under the vercel_live build tag, so nothing in
// an ordinary build can reach them.

// LiveSandbox creates a sandbox for runnerID with rules as its policy, and
// returns its session. The caller destroys it with Destroy.
func (b *Backend) LiveSandbox(ctx context.Context, runnerID uuid.UUID, rules []application.EgressRule) (string, error) {
	b.api.timeout = 5 * time.Minute
	policy, err := buildPolicy(rules)
	if err != nil {
		return "", err
	}
	created, err := b.create(ctx, b.sandboxName(runnerID), application.RunnerSpec{RunnerID: runnerID, SessionID: uuid.New()}, policy)
	if err != nil {
		return "", err
	}
	return created.Sandbox.CurrentSessionID, nil
}

// LiveRun runs a script in the sandbox's session and returns its exit code and
// output.
func (b *Backend) LiveRun(ctx context.Context, session, script string) (int, string, error) {
	var started commandResponse
	if err := b.api.do(ctx, request{method: http.MethodPost, path: "/v2/sandboxes/sessions/" + url.PathEscape(session) + "/cmd",
		body: commandRequest{Command: "bash", Args: []string{"-c", script}}, accept: []int{http.StatusOK}}, &started); err != nil {
		return 0, "", err
	}
	exit, err := b.commandExitWait(ctx, session, started.Command.ID)
	if err != nil {
		return 0, "", err
	}
	return exit, b.liveOutput(ctx, session, started.Command.ID), nil
}

// LiveSnapshots lists the snapshots kept for a runner's sandbox.
func (b *Backend) LiveSnapshots(ctx context.Context, runnerID uuid.UUID) ([]string, error) {
	return b.snapshotsOf(ctx, b.sandboxName(runnerID))
}

func (b *Backend) commandExitWait(ctx context.Context, session, command string) (int, error) {
	var done commandResponse
	if err := b.api.do(ctx, request{method: http.MethodGet,
		path:  "/v2/sandboxes/sessions/" + url.PathEscape(session) + "/cmd/" + url.PathEscape(command),
		query: url.Values{"wait": []string{"true"}}, accept: []int{http.StatusOK}}, &done); err != nil {
		return 0, err
	}
	if done.Command.ExitCode == nil {
		return -1, nil
	}
	return *done.Command.ExitCode, nil
}

func (b *Backend) liveOutput(ctx context.Context, session, command string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.api.base+"/v2/sandboxes/sessions/"+session+"/cmd/"+command+
		"/logs?teamId="+b.api.teamID, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+b.api.token)
	resp, err := b.api.http.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	var out strings.Builder
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	for {
		var line struct {
			Data string `json:"data"`
		}
		if decoder.Decode(&line) != nil {
			break
		}
		out.WriteString(line.Data)
	}
	return out.String()
}
