package application_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

type fakeRegistryStore struct {
	runners  map[uuid.UUID]application.RegistryRunner
	recorded []application.RegistryRequest
	tenants  []uuid.UUID
	fail     bool
}

type registryTenantKey struct{}

func (s *fakeRegistryStore) RunnerForRegistryRequest(_ context.Context, id uuid.UUID) (application.RegistryRunner, error) {
	runner, ok := s.runners[id]
	if !ok {
		return application.RegistryRunner{}, domain.ErrRunnerNotFound
	}
	return runner, nil
}

func (s *fakeRegistryStore) RecordRegistryRequest(ctx context.Context, request application.RegistryRequest) error {
	if s.fail {
		return errors.New("database unavailable")
	}
	s.tenants = append(s.tenants, ctx.Value(registryTenantKey{}).(uuid.UUID))
	s.recorded = append(s.recorded, request)
	return nil
}

func newRecorder(store *fakeRegistryStore) *application.RegistryRecorder {
	return application.NewRegistryRecorder(store, func(ctx context.Context, workspace uuid.UUID) context.Context {
		return context.WithValue(ctx, registryTenantKey{}, workspace)
	}, "vercel-", func() time.Time { return time.Unix(1790000000, 0) })
}

// TestARegistryRequestIsRecordedForItsRunnersSession: the session and
// workspace come from the runner row, never from the request, and the write
// runs in that workspace's tenant context. The query string is never kept.
func TestARegistryRequestIsRecordedForItsRunnersSession(t *testing.T) {
	runner, session, workspace := uuid.New(), uuid.New(), uuid.New()
	store := &fakeRegistryStore{runners: map[uuid.UUID]application.RegistryRunner{
		runner: {SessionID: session, WorkspaceID: workspace, Backend: "vercel-live", State: domain.RunnerRunning},
	}}
	got, err := newRecorder(store).Record(context.Background(), runner, "registry.npmjs.org", "get",
		"/left-pad/-/left-pad-1.3.0.tgz?token=secret")
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID != session || got.WorkspaceID != workspace || got.RunnerID != runner || got.Method != "GET" ||
		got.Path != "/left-pad/-/left-pad-1.3.0.tgz" || got.Host != "registry.npmjs.org" {
		t.Errorf("recorded %+v", got)
	}
	if len(store.recorded) != 1 || store.tenants[0] != workspace {
		t.Errorf("recorded %d rows in tenants %v; want one, in the runner's workspace", len(store.recorded), store.tenants)
	}
}

// TestARegistryRequestIsRefusedWhenItCannotBeTrusted: an unknown runner, an
// ended one, one from a backend that does not forward, a host off the
// registry list, a relative path, an unknown method, and a record that cannot
// be written — every one an error, which the proxy turns into a refusal.
func TestARegistryRequestIsRefusedWhenItCannotBeTrusted(t *testing.T) {
	live, ended, docker := uuid.New(), uuid.New(), uuid.New()
	store := &fakeRegistryStore{runners: map[uuid.UUID]application.RegistryRunner{
		live:   {SessionID: uuid.New(), WorkspaceID: uuid.New(), Backend: "vercel-live", State: domain.RunnerRunning},
		ended:  {SessionID: uuid.New(), WorkspaceID: uuid.New(), Backend: "vercel-live", State: domain.RunnerTerminated},
		docker: {SessionID: uuid.New(), WorkspaceID: uuid.New(), Backend: "dev-docker-dev", State: domain.RunnerRunning},
	}}
	recorder := newRecorder(store)
	for name, c := range map[string]struct {
		runner             uuid.UUID
		host, method, path string
		want               error
	}{
		"unknown runner":        {uuid.New(), "registry.npmjs.org", "GET", "/x", domain.ErrRunnerNotFound},
		"ended runner":          {ended, "registry.npmjs.org", "GET", "/x", application.ErrRegistryRunnerNotLive},
		"non-sandbox backend":   {docker, "registry.npmjs.org", "GET", "/x", application.ErrRegistryRunnerNotLive},
		"host off the list":     {live, "example.com", "GET", "/x", application.ErrRegistryRequestInvalid},
		"git is not a registry": {live, "github.com", "GET", "/x", application.ErrRegistryRequestInvalid},
		"relative path":         {live, "registry.npmjs.org", "GET", "x", application.ErrRegistryRequestInvalid},
		"unknown method":        {live, "registry.npmjs.org", "CONNECT", "/x", application.ErrRegistryRequestInvalid},
	} {
		if _, err := recorder.Record(context.Background(), c.runner, c.host, c.method, c.path); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	if len(store.recorded) != 0 {
		t.Errorf("%d refused requests were recorded", len(store.recorded))
	}
	store.fail = true
	if _, err := recorder.Record(context.Background(), live, "registry.npmjs.org", "GET", "/x"); err == nil {
		t.Error("a record that could not be written reported success; the proxy would forward it unrecorded")
	}
}

// TestAPathIsRecordedExactlyOrRefused is a review finding on PR #26: a path
// was cut to 2 KiB for the record while the whole was forwarded, so two
// different fetches could read the same. Now the record is exactly what is
// forwarded, less its query, or the request is refused.
func TestAPathIsRecordedExactlyOrRefused(t *testing.T) {
	runner := uuid.New()
	store := &fakeRegistryStore{runners: map[uuid.UUID]application.RegistryRunner{
		runner: {SessionID: uuid.New(), WorkspaceID: uuid.New(), Backend: "vercel-live", State: domain.RunnerProvisioning},
	}}
	recorder := newRecorder(store)
	atCap := "/" + strings.Repeat("a", application.MaxRegistryPathBytes-1)
	got, err := recorder.Record(context.Background(), runner, "pypi.org", "GET", atCap+"?q=1")
	if err != nil || got.Path != atCap {
		t.Errorf("a path at the cap: %d bytes recorded, %v; want it whole", len(got.Path), err)
	}
	for name, path := range map[string]string{
		"one byte over the cap": atCap + "a",
		"not valid UTF-8":       "/left-pad\xff",
		"a NUL":                 "/left\x00pad",
	} {
		if _, err := recorder.Record(context.Background(), runner, "pypi.org", "GET", path); !errors.Is(err, application.ErrRegistryRequestInvalid) {
			t.Errorf("%s: %v, want refused rather than rewritten", name, err)
		}
	}
	if len(store.recorded) != 1 {
		t.Errorf("%d rows; only the path at the cap should be recorded", len(store.recorded))
	}
	if !utf8.ValidString(got.Path) {
		t.Error("the recorded path is not valid UTF-8")
	}
}

// TestAProxyBaseWithAPathIsRefused is a review finding on PR #26: the proxy
// serves /r/<host> at its root, so a base under a prefix would route every
// registry request to a 404.
func TestAProxyBaseWithAPathIsRefused(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://registry.weave.example":         true,
		"https://registry.weave.example/":        true,
		"https://registry.weave.example/prefix":  false,
		"https://registry.weave.example/r":       false,
		"http://registry.weave.example":          false,
		"https://u:p@registry.weave.example":     false,
		"https://registry.weave.example/?q=1":    false,
		"https://registry.weave.example/#anchor": false,
	} {
		if err := application.ValidateRegistryProxyURL(raw); (err == nil) != ok {
			t.Errorf("%q: %v, want ok=%v", raw, err, ok)
		}
	}
}
