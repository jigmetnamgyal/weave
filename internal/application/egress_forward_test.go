package application_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// withEgress rewires a runner world's service with the given proxy and reserved
// namespaces, keeping every other collaborator.
func withEgress(t *testing.T, w *runnerWorld, proxy string, reserved []string) {
	t.Helper()
	w.service = application.NewRunnerService(w.store, fakeSessionReader{w}, w.backend, fakeMinter{},
		fakeBrokerIssuer{}, "nats://broker.test:4222", fakeAgents{}, w.drain,
		func(ctx context.Context, _ uuid.UUID) context.Context { return ctx }, "https://github.test",
		slog.New(slog.NewTextHandler(io.Discard, nil))).WithEgress(w.snapshots(), proxy, reserved)
}

// TestAddedHostsAreForwardedNeverPlain: each snapshot host becomes a rule
// forwarded to the egress proxy's route for exactly that host, alongside the
// unchanged defaults.
func TestAddedHostsAreForwardedNeverPlain(t *testing.T) {
	w := newRunnerTestWorld(t)
	w.added = []string{"docs.example.com", "status.example.org"}
	withEgress(t, w, "https://egress.weave.example", nil)
	if _, err := w.service.Provision(context.Background(), w.session.WorkspaceID, w.session.ID); err != nil {
		t.Fatal(err)
	}
	rules := map[string]string{}
	for _, rule := range w.backend.lastSpec.Egress {
		rules[rule.Host] = rule.ForwardURL
	}
	for _, host := range w.added {
		if rules[host] != "https://egress.weave.example/e/"+host {
			t.Errorf("%s: forwardURL %q, want the egress proxy's route for it", host, rules[host])
		}
	}
	if _, ok := rules["github.com"]; !ok {
		t.Error("the default rules were dropped")
	}
}

// TestProvisioningFailsClosedOnEgress: a missing snapshot, added hosts with no
// proxy, a host that is now reserved, or no snapshot wiring at all each stop
// provisioning before the backend is asked for anything.
func TestProvisioningFailsClosedOnEgress(t *testing.T) {
	for name, setup := range map[string]func(*runnerWorld){
		"missing snapshot": func(w *runnerWorld) { w.missing = true; withEgress(t, w, "https://egress.weave.example", nil) },
		"no proxy URL":     func(w *runnerWorld) { w.added = []string{"docs.example.com"}; withEgress(t, w, "", nil) },
		"now reserved": func(w *runnerWorld) {
			w.added = []string{"api.weave.example.com"}
			withEgress(t, w, "https://egress.weave.example", []string{"weave.example.com"})
		},
		"not canonical": func(w *runnerWorld) {
			w.added = []string{"DOCS.example.com"}
			withEgress(t, w, "https://egress.weave.example", nil)
		},
		"not wired": func(w *runnerWorld) {
			w.service = application.NewRunnerService(w.store, fakeSessionReader{w}, w.backend, fakeMinter{},
				fakeBrokerIssuer{}, "nats://broker.test:4222", fakeAgents{}, w.drain,
				func(ctx context.Context, _ uuid.UUID) context.Context { return ctx }, "https://github.test",
				slog.New(slog.NewTextHandler(io.Discard, nil)))
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newRunnerTestWorld(t)
			setup(w)
			if _, err := w.service.Provision(context.Background(), w.session.WorkspaceID, w.session.ID); err == nil {
				t.Fatal("provisioned anyway")
			}
			if w.backend.provisioned != 0 {
				t.Errorf("the backend was asked for an environment %d times", w.backend.provisioned)
			}
		})
	}
	// An empty snapshot with no proxy is fine: there is nothing to forward.
	w := newRunnerTestWorld(t)
	if _, err := w.service.Provision(context.Background(), w.session.WorkspaceID, w.session.ID); err != nil {
		t.Errorf("an empty snapshot without a proxy: %v", err)
	}
}

// fakeLookup and fakeReader drive the egress authorizer.
type fakeLookup struct {
	runner application.RegistryRunner
	err    error
}

func (f fakeLookup) RunnerForRegistryRequest(context.Context, uuid.UUID) (application.RegistryRunner, error) {
	return f.runner, f.err
}

type fakeReader struct {
	snapshot domain.RunnerEgressSnapshot
	err      error
}

type egressTenantKey struct{}

func (f fakeReader) Snapshot(ctx context.Context, workspaceID, _ uuid.UUID) (domain.RunnerEgressSnapshot, error) {
	if got, _ := ctx.Value(egressTenantKey{}).(uuid.UUID); got != workspaceID {
		return domain.RunnerEgressSnapshot{}, errors.New("read outside the runner's own tenant")
	}
	return f.snapshot, f.err
}

// TestTheEgressAuthorizerUsesTheRunnersOwnSnapshot: only a live forwarding
// runner, only its snapshot, read in its own tenant, and only an exact host.
func TestTheEgressAuthorizerUsesTheRunnersOwnSnapshot(t *testing.T) {
	runnerID, workspace := uuid.New(), uuid.New()
	live := application.RegistryRunner{WorkspaceID: workspace, SessionID: uuid.New(), Backend: "vercel-live", State: domain.RunnerRunning}
	snapshot := domain.RunnerEgressSnapshot{RunnerID: runnerID, WorkspaceID: workspace, Hosts: []string{"docs.example.com"}}
	bind := func(ctx context.Context, id uuid.UUID) context.Context {
		return context.WithValue(ctx, egressTenantKey{}, id)
	}
	authorize := func(lookup fakeLookup, reader fakeReader, host string) error {
		_, err := application.NewEgressAuthorizer(lookup, reader, bind, "vercel-").Authorize(context.Background(), runnerID, host)
		return err
	}
	if err := authorize(fakeLookup{runner: live}, fakeReader{snapshot: snapshot}, "docs.example.com"); err != nil {
		t.Fatalf("an authorized host: %v", err)
	}
	ended := live
	ended.State = domain.RunnerTerminated
	dev := live
	dev.Backend = "dev-docker"
	other := snapshot
	other.RunnerID = uuid.New()
	for name, err := range map[string]error{
		"a subdomain":          authorize(fakeLookup{runner: live}, fakeReader{snapshot: snapshot}, "api.docs.example.com"),
		"another host":         authorize(fakeLookup{runner: live}, fakeReader{snapshot: snapshot}, "example.com"),
		"an ended runner":      authorize(fakeLookup{runner: ended}, fakeReader{snapshot: snapshot}, "docs.example.com"),
		"a non-forwarding one": authorize(fakeLookup{runner: dev}, fakeReader{snapshot: snapshot}, "docs.example.com"),
		"an unknown runner":    authorize(fakeLookup{err: domain.ErrRunnerNotFound}, fakeReader{snapshot: snapshot}, "docs.example.com"),
		"a missing snapshot":   authorize(fakeLookup{runner: live}, fakeReader{err: application.ErrEgressSnapshotMissing}, "docs.example.com"),
		"another runner's":     authorize(fakeLookup{runner: live}, fakeReader{snapshot: other}, "docs.example.com"),
		"a database failure":   authorize(fakeLookup{runner: live}, fakeReader{err: errors.New("connection refused")}, "docs.example.com"),
	} {
		if err == nil {
			t.Errorf("%s was authorized", name)
		}
	}
}
