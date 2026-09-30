package proxy_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/adapters/vercelsandbox"
	"github.com/jigmetnamgyal/weave/internal/adapters/vercelsandbox/vercelsandboxtest"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
	"github.com/jigmetnamgyal/weave/services/registry-proxy/internal/proxy"
)

const (
	team    = "team_test"
	project = "prj_runners"
	base    = "https://proxy.example.test"
)

type recorded struct {
	runner             uuid.UUID
	host, method, path string
}

type fakeRecorder struct {
	mu    sync.Mutex
	calls []recorded
	err   error
}

func (f *fakeRecorder) Record(_ context.Context, runner uuid.UUID, host, method, path string) (application.RegistryRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, recorded{runner, host, method, path})
	if f.err != nil {
		return application.RegistryRequest{}, f.err
	}
	p, _, _ := strings.Cut(path, "?")
	return application.RegistryRequest{SessionID: uuid.New(), Method: method, Path: p}, nil
}

// upstream is a fake registry that records what reached it.
type upstream struct {
	*httptest.Server
	mu       sync.Mutex
	requests []*http.Request
	bodies   []string
	handler  http.HandlerFunc
}

func newUpstream(t *testing.T) *upstream {
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.requests = append(u.requests, r.Clone(context.Background()))
		u.bodies = append(u.bodies, string(body))
		handler := u.handler
		u.mu.Unlock()
		if handler != nil {
			handler(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Registry", "yes")
		_, _ = io.WriteString(w, `{"name":"left-pad"}`)
	}))
	t.Cleanup(u.Close)
	return u
}

type world struct {
	issuer   *vercelsandboxtest.Issuer
	recorder *fakeRecorder
	upstream *upstream
	handler  http.Handler
	runner   uuid.UUID
}

func newWorld(t *testing.T) *world {
	t.Helper()
	issuer := vercelsandboxtest.NewIssuer(t, team)
	verifier, err := vercelsandbox.NewOIDCVerifier(context.Background(), vercelsandbox.OIDCConfig{
		TeamID: team, ProjectID: project, IssuerBase: issuer.Server.URL, HTTPClient: issuer.Server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	w := &world{issuer: issuer, recorder: &fakeRecorder{}, upstream: newUpstream(t), runner: uuid.New()}
	w.handler = proxy.New(proxy.Config{
		PublicBase: base, Verifier: verifier, Recorder: w.recorder,
		UpstreamURL: func(string) string { return w.upstream.URL },
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return w
}

// token mints a token as Vercel would for this world's runner on a route.
func (w *world) token(t *testing.T, routeHost string) string {
	return w.issuer.Mint(t, vercelsandboxtest.Token{Audience: []string{base + "/r/" + routeHost}, ProjectID: project,
		SandboxName: "weave-runner-live-" + w.runner.String()})
}

// forwarded builds a request as Vercel's firewall delivers it.
func forwarded(method, host, path, token string, body io.Reader) *http.Request {
	pathOnly, _, _ := strings.Cut(path, "?")
	r := httptest.NewRequest(method, "/r/"+host+pathOnly, body)
	if _, query, ok := strings.Cut(path, "?"); ok {
		r.URL.RawQuery = query
	}
	r.Header.Set("Vercel-Sandbox-Oidc-Token", token)
	r.Header.Set("Vercel-Forwarded-Host", host)
	r.Header.Set("Vercel-Forwarded-Path", path)
	r.Header.Set("Vercel-Forwarded-Scheme", "https")
	r.Header.Set("Vercel-Forwarded-Port", "443")
	r.Header.Set("Cf-Connecting-Ip", "44.211.52.79")
	r.Header.Set("X-Forwarded-For", "44.211.52.79")
	r.Header.Set("Cdn-Loop", "cloudflare")
	r.Header.Set("User-Agent", "npm/11.19.0")
	return r
}

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// TestARegistryRequestIsRecordedThenForwarded: the happy path, and everything
// about what reaches the registry and what comes back.
func TestARegistryRequestIsRecordedThenForwarded(t *testing.T) {
	w := newWorld(t)
	r := forwarded(http.MethodPost, "registry.npmjs.org", "/-/npm/v1/security/audits/quick?x=1",
		w.token(t, "registry.npmjs.org"), strings.NewReader(`{"audit":true}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Connection", "X-Hop")
	r.Header.Set("X-Hop", "connection-scoped")
	rec := serve(w.handler, r)

	if rec.Code != http.StatusOK || rec.Body.String() != `{"name":"left-pad"}` || rec.Header().Get("X-Registry") != "yes" {
		t.Fatalf("response %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
	if rec.Header().Get("Connection") != "" {
		t.Error("a hop-by-hop response header was passed back")
	}
	if len(w.recorder.calls) != 1 {
		t.Fatalf("%d records, want 1", len(w.recorder.calls))
	}
	if got := w.recorder.calls[0]; got.runner != w.runner || got.host != "registry.npmjs.org" || got.method != "POST" {
		t.Errorf("recorded %+v", got)
	}
	if len(w.upstream.requests) != 1 {
		t.Fatalf("%d upstream requests, want 1", len(w.upstream.requests))
	}
	up := w.upstream.requests[0]
	if up.Method != "POST" || up.URL.Path != "/-/npm/v1/security/audits/quick" || up.URL.RawQuery != "x=1" ||
		up.Host != "registry.npmjs.org" || w.upstream.bodies[0] != `{"audit":true}` || up.Header.Get("User-Agent") != "npm/11.19.0" {
		t.Errorf("upstream got %s %s host %q body %q", up.Method, up.URL, up.Host, w.upstream.bodies[0])
	}
	for name := range up.Header {
		for _, banned := range []string{"Vercel-", "Cf-", "Cdn-Loop", "X-Forwarded-", "X-Hop"} {
			if strings.HasPrefix(name, banned) {
				t.Errorf("%s reached the registry; the sandbox token or the path's headers must not", name)
			}
		}
	}
}

// TestNothingIsForwardedOrRecordedWithoutAValidTokenForThisRoute: refused
// before the database or the registry is touched.
func TestNothingIsForwardedOrRecordedWithoutAValidTokenForThisRoute(t *testing.T) {
	w := newWorld(t)
	notRunner := w.issuer.Mint(t, vercelsandboxtest.Token{Audience: []string{base + "/r/registry.npmjs.org"},
		ProjectID: project, SandboxName: "someone-elses-sandbox"})
	for name, c := range map[string]struct {
		token string
		want  int
	}{
		"no token":                         {"", http.StatusUnauthorized},
		"a forged token":                   {w.issuer.Mint(t, vercelsandboxtest.Token{Audience: []string{base + "/r/registry.npmjs.org"}, ProjectID: project, SandboxName: "weave-runner-live-" + w.runner.String(), Key: vercelsandboxtest.ForeignKey(t)}), http.StatusUnauthorized},
		"a token for another registry":     {w.token(t, "pypi.org"), http.StatusUnauthorized},
		"another project's sandbox":        {w.issuer.Mint(t, vercelsandboxtest.Token{Audience: []string{base + "/r/registry.npmjs.org"}, ProjectID: "prj_other", SandboxName: "weave-runner-live-" + w.runner.String()}), http.StatusUnauthorized},
		"a sandbox that is not a runner's": {notRunner, http.StatusForbidden},
	} {
		rec := serve(w.handler, forwarded(http.MethodGet, "registry.npmjs.org", "/left-pad", c.token, nil))
		if rec.Code != c.want {
			t.Errorf("%s: %d, want %d", name, rec.Code, c.want)
		}
	}
	if len(w.recorder.calls) != 0 || len(w.upstream.requests) != 0 {
		t.Errorf("%d records and %d upstream requests for refused requests; want none", len(w.recorder.calls), len(w.upstream.requests))
	}
}

// TestTheForwardedDestinationMustBeTheRoutesRegistry: every Vercel-Forwarded-*
// header is a claim checked against the route; the proxy is not an open proxy.
func TestTheForwardedDestinationMustBeTheRoutesRegistry(t *testing.T) {
	w := newWorld(t)
	good := w.token(t, "registry.npmjs.org")
	for name, mutate := range map[string]func(*http.Request){
		"another host":      func(r *http.Request) { r.Header.Set("Vercel-Forwarded-Host", "example.com") },
		"plain http":        func(r *http.Request) { r.Header.Set("Vercel-Forwarded-Scheme", "http") },
		"port 80":           func(r *http.Request) { r.Header.Set("Vercel-Forwarded-Port", "80") },
		"a different path":  func(r *http.Request) { r.Header.Set("Vercel-Forwarded-Path", "/other-package") },
		"a relative path":   func(r *http.Request) { r.Header.Set("Vercel-Forwarded-Path", "left-pad") },
		"no forwarded path": func(r *http.Request) { r.Header.Del("Vercel-Forwarded-Path") },
	} {
		r := forwarded(http.MethodGet, "registry.npmjs.org", "/left-pad", good, nil)
		mutate(r)
		if rec := serve(w.handler, r); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, rec.Code)
		}
	}
	// A route for a host off the registry list, with a token genuinely issued
	// for it: still refused — the proxy fetches from its list and nothing else.
	off := forwarded(http.MethodGet, "example.com", "/", w.token(t, "example.com"), nil)
	if rec := serve(w.handler, off); rec.Code != http.StatusBadRequest {
		t.Errorf("a route off the registry list: %d, want 400", rec.Code)
	}
	if len(w.recorder.calls) != 0 || len(w.upstream.requests) != 0 {
		t.Errorf("%d records and %d upstream requests; want none", len(w.recorder.calls), len(w.upstream.requests))
	}
}

// TestARequestThatCannotBeRecordedIsNotForwarded: the control fails closed.
func TestARequestThatCannotBeRecordedIsNotForwarded(t *testing.T) {
	for name, c := range map[string]struct {
		err  error
		want int
	}{
		"the runner is unknown":   {domain.ErrRunnerNotFound, http.StatusForbidden},
		"the runner has ended":    {application.ErrRegistryRunnerNotLive, http.StatusForbidden},
		"the database is down":    {errors.New("connection refused"), http.StatusServiceUnavailable},
		"the record is malformed": {application.ErrRegistryRequestInvalid, http.StatusBadRequest},
	} {
		w := newWorld(t)
		w.recorder.err = c.err
		rec := serve(w.handler, forwarded(http.MethodGet, "registry.npmjs.org", "/left-pad", w.token(t, "registry.npmjs.org"), nil))
		if rec.Code != c.want || len(w.upstream.requests) != 0 {
			t.Errorf("%s: %d with %d upstream requests; want %d and none", name, rec.Code, len(w.upstream.requests), c.want)
		}
	}
}

// TestARedirectIsReturnedNotFollowed: the sandbox's own policy decides whether
// a redirect's target is reachable; the proxy never fetches from it.
func TestARedirectIsReturnedNotFollowed(t *testing.T) {
	w := newWorld(t)
	w.upstream.handler = func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Location", "https://attacker.example/steal")
		rw.WriteHeader(http.StatusFound)
	}
	rec := serve(w.handler, forwarded(http.MethodGet, "registry.npmjs.org", "/left-pad", w.token(t, "registry.npmjs.org"), nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "https://attacker.example/steal" || len(w.upstream.requests) != 1 {
		t.Errorf("= %d, Location %q, %d upstream requests; want the redirect returned and nothing followed",
			rec.Code, rec.Header().Get("Location"), len(w.upstream.requests))
	}
}

// TestAnUnreachableRegistryIsABadGatewayAfterTheRecord: the request was
// recorded before the fetch failed, which is the order that makes the record
// complete.
func TestAnUnreachableRegistryIsABadGatewayAfterTheRecord(t *testing.T) {
	w := newWorld(t)
	w.upstream.Close()
	rec := serve(w.handler, forwarded(http.MethodGet, "registry.npmjs.org", "/left-pad", w.token(t, "registry.npmjs.org"), nil))
	if rec.Code != http.StatusBadGateway || len(w.recorder.calls) != 1 {
		t.Errorf("= %d with %d records; want 502, recorded", rec.Code, len(w.recorder.calls))
	}
}

// TestUnverifiableTokensAreAnOutage: keys unreachable is 503, not 401.
func TestUnverifiableTokensAreAnOutage(t *testing.T) {
	issuer := vercelsandboxtest.NewIssuer(t, team)
	verifier, err := vercelsandbox.NewOIDCVerifier(context.Background(), vercelsandbox.OIDCConfig{
		TeamID: team, ProjectID: project, IssuerBase: "http://127.0.0.1:1", LookupTimeout: 200_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := &fakeRecorder{}
	h := proxy.New(proxy.Config{PublicBase: base, Verifier: verifier, Recorder: recorder,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	token := issuer.Mint(t, vercelsandboxtest.Token{Audience: []string{base + "/r/registry.npmjs.org"}, ProjectID: project,
		SandboxName: "weave-runner-live-" + uuid.NewString()})
	if rec := serve(h, forwarded(http.MethodGet, "registry.npmjs.org", "/left-pad", token, nil)); rec.Code != http.StatusServiceUnavailable || len(recorder.calls) != 0 {
		t.Errorf("= %d with %d records; want 503, none", rec.Code, len(recorder.calls))
	}
}

// TestAScopedPackageIsForwardedInItsEncodedForm is a review finding on PR #26:
// npm requests a scoped package as /@scope%2fname. The route was compared in
// Go's decoded form against the encoded Vercel-Forwarded-Path, so every
// scoped package was refused.
func TestAScopedPackageIsForwardedInItsEncodedForm(t *testing.T) {
	w := newWorld(t)
	r := httptest.NewRequest(http.MethodGet, "/r/registry.npmjs.org/@types%2fnode", nil)
	for k, v := range forwarded(http.MethodGet, "registry.npmjs.org", "/@types%2fnode", w.token(t, "registry.npmjs.org"), nil).Header {
		r.Header[k] = v
	}
	rec := serve(w.handler, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("a scoped package: %d %s", rec.Code, rec.Body.String())
	}
	if len(w.recorder.calls) != 1 || w.recorder.calls[0].path != "/@types%2fnode" {
		t.Errorf("recorded %+v; want the encoded path", w.recorder.calls)
	}
	if len(w.upstream.requests) != 1 || w.upstream.requests[0].URL.EscapedPath() != "/@types%2fnode" {
		t.Errorf("the registry was asked for %v; want the encoded path", w.upstream.requests)
	}
}

// TestAnOversizedBodyIsRefusedBeforeAnythingIsSent is a review finding on
// PR #26: the limit tripped while the body streamed upstream, after the record
// and after part of it had reached the registry.
func TestAnOversizedBodyIsRefusedBeforeAnythingIsSent(t *testing.T) {
	for name, chunked := range map[string]bool{"declared length": false, "chunked, length unknown": true} {
		w := newWorld(t)
		r := forwarded(http.MethodPost, "registry.npmjs.org", "/-/npm/v1/security/audits/quick", w.token(t, "registry.npmjs.org"),
			strings.NewReader(strings.Repeat("x", proxy.MaxRequestBody+1)))
		if chunked {
			r.ContentLength = -1
		}
		rec := serve(w.handler, r)
		if rec.Code != http.StatusRequestEntityTooLarge || len(w.recorder.calls) != 0 || len(w.upstream.requests) != 0 {
			t.Errorf("%s: %d with %d records and %d upstream requests; want 413 and nothing",
				name, rec.Code, len(w.recorder.calls), len(w.upstream.requests))
		}
	}
	// And a body at the limit goes through whole.
	w := newWorld(t)
	body := strings.Repeat("x", proxy.MaxRequestBody)
	rec := serve(w.handler, forwarded(http.MethodPost, "registry.npmjs.org", "/-/npm/v1/security/audits/quick",
		w.token(t, "registry.npmjs.org"), strings.NewReader(body)))
	if rec.Code != http.StatusOK || len(w.upstream.bodies) != 1 || len(w.upstream.bodies[0]) != len(body) {
		t.Errorf("a body at the limit: %d; want it forwarded whole", rec.Code)
	}
}
