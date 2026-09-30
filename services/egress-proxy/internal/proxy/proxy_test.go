package proxy_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/adapters/guardedhttp"
	"github.com/jigmetnamgyal/weave/internal/adapters/vercelsandbox"
	"github.com/jigmetnamgyal/weave/internal/adapters/vercelsandbox/vercelsandboxtest"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/services/egress-proxy/internal/proxy"
)

const (
	team    = "team_test"
	project = "prj_runners"
	base    = "https://egress.example.test"
	host    = "docs.example.com"
)

// fakeAuthorizer allows exactly one runner and host set, and records calls.
type fakeAuthorizer struct {
	mu      sync.Mutex
	runner  uuid.UUID
	allowed map[string]bool
	err     error
	calls   int
}

func (f *fakeAuthorizer) Authorize(_ context.Context, runner uuid.UUID, h string) (application.RegistryRunner, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return application.RegistryRunner{}, f.err
	}
	if runner != f.runner || !f.allowed[h] {
		return application.RegistryRunner{}, application.ErrEgressHostNotAuthorized
	}
	return application.RegistryRunner{SessionID: uuid.New()}, nil
}

// fakeUpstream records what would have reached the origin.
type fakeUpstream struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   []string
	respond  func(*http.Request) (*http.Response, error)
}

func (f *fakeUpstream) Do(r *http.Request) (*http.Response, error) {
	body := ""
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}
	f.mu.Lock()
	f.requests = append(f.requests, r)
	f.bodies = append(f.bodies, body)
	respond := f.respond
	f.mu.Unlock()
	if respond != nil {
		return respond(r)
	}
	h := http.Header{}
	h.Set("Content-Type", "text/plain")
	h.Set("Vercel-Sandbox-Oidc-Token", "must-not-return")
	h.Set("Connection", "close")
	return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader("origin says hi"))}, nil
}

type world struct {
	issuer     *vercelsandboxtest.Issuer
	verifier   *vercelsandbox.OIDCVerifier
	authorizer *fakeAuthorizer
	upstream   *fakeUpstream
	handler    http.Handler
	runner     uuid.UUID
}

func newWorld(t *testing.T, up proxy.Upstream) *world {
	t.Helper()
	issuer := vercelsandboxtest.NewIssuer(t, team)
	verifier, err := vercelsandbox.NewOIDCVerifier(context.Background(), vercelsandbox.OIDCConfig{
		TeamID: team, ProjectID: project, IssuerBase: issuer.Server.URL, HTTPClient: issuer.Server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	w := &world{issuer: issuer, verifier: verifier, runner: uuid.New(), upstream: &fakeUpstream{}}
	w.authorizer = &fakeAuthorizer{runner: w.runner, allowed: map[string]bool{host: true, "status.example.org": true}}
	if up == nil {
		up = w.upstream
	}
	w.handler = proxy.New(proxy.Config{PublicBase: base, Verifier: verifier, Authorizer: w.authorizer,
		Upstream: up, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), BodyReadTimeout: 300 * time.Millisecond})
	return w
}

// token mints a token as Vercel would for this world's runner on a route.
func (w *world) token(t *testing.T, routeHost string) string {
	return w.issuer.Mint(t, vercelsandboxtest.Token{Audience: []string{base + "/e/" + routeHost}, ProjectID: project,
		SandboxName: "weave-runner-live-" + w.runner.String()})
}

// forwarded builds a request as Vercel's firewall delivers it.
func forwarded(method, h, path, token string, body io.Reader) *http.Request {
	pathOnly, _, _ := strings.Cut(path, "?")
	r := httptest.NewRequest(method, "/e/"+h+pathOnly, body)
	if _, query, ok := strings.Cut(path, "?"); ok {
		r.URL.RawQuery = query
	}
	r.Header.Set("Vercel-Sandbox-Oidc-Token", token)
	r.Header.Set("Vercel-Forwarded-Host", h)
	r.Header.Set("Vercel-Forwarded-Path", path)
	r.Header.Set("Vercel-Forwarded-Scheme", "https")
	r.Header.Set("Vercel-Forwarded-Port", "443")
	r.Header.Set("X-Forwarded-For", "44.211.52.79")
	r.Header.Set("Cf-Connecting-Ip", "44.211.52.79")
	r.Header.Set("Accept", "text/plain")
	return r
}

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// TestAnAuthorizedRequestIsForwardedClean: the origin gets the request at its
// own URL with the sandbox's headers, and none of the headers added on the way
// — the token above all. The response comes back without them either.
func TestAnAuthorizedRequestIsForwardedClean(t *testing.T) {
	w := newWorld(t, nil)
	rec := serve(w.handler, forwarded("GET", host, "/guide?page=2", w.token(t, host), nil))
	if rec.Code != 200 || rec.Body.String() != "origin says hi" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	if len(w.upstream.requests) != 1 {
		t.Fatalf("%d upstream requests", len(w.upstream.requests))
	}
	sent := w.upstream.requests[0]
	if sent.URL.String() != "https://docs.example.com/guide?page=2" || sent.Header.Get("Accept") != "text/plain" {
		t.Errorf("forwarded %s with Accept %q", sent.URL, sent.Header.Get("Accept"))
	}
	for _, name := range []string{"Vercel-Sandbox-Oidc-Token", "Vercel-Forwarded-Host", "X-Forwarded-For", "Cf-Connecting-Ip"} {
		if sent.Header.Get(name) != "" {
			t.Errorf("%s reached the origin", name)
		}
	}
	if rec.Header().Get("Vercel-Sandbox-Oidc-Token") != "" || rec.Header().Get("Connection") != "" {
		t.Error("an origin's hop-by-hop or Vercel header was passed back")
	}
}

// TestEveryRefusalHappensBeforeTheOrigin walks the request order: each failed
// step refuses with its status, and nothing reaches the origin.
func TestEveryRefusalHappensBeforeTheOrigin(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		build  func(t *testing.T, w *world) *http.Request
	}{
		{"no route", 404, func(t *testing.T, w *world) *http.Request {
			r := forwarded("GET", host, "/", w.token(t, host), nil)
			r.URL.Path, r.URL.RawPath = "/r/"+host+"/", ""
			return r
		}},
		{"a raw IP route", 404, func(t *testing.T, w *world) *http.Request {
			return forwarded("GET", "10.0.0.1", "/", w.token(t, "10.0.0.1"), nil)
		}},
		{"no token", 401, func(t *testing.T, w *world) *http.Request { return forwarded("GET", host, "/", "", nil) }},
		{"a token for another host", 401, func(t *testing.T, w *world) *http.Request {
			return forwarded("GET", host, "/", w.token(t, "other.example.com"), nil)
		}},
		// Replay across routes: both hosts are in the snapshot, but a token
		// Vercel issued for one route must not open another.
		{"a token replayed on another authorized route", 401, func(t *testing.T, w *world) *http.Request {
			return forwarded("GET", "status.example.org", "/", w.token(t, host), nil)
		}},
		{"a token for the registry route", 401, func(t *testing.T, w *world) *http.Request {
			tok := w.issuer.Mint(t, vercelsandboxtest.Token{Audience: []string{base + "/r/" + host}, ProjectID: project,
				SandboxName: "weave-runner-live-" + w.runner.String()})
			return forwarded("GET", host, "/", tok, nil)
		}},
		{"not a runner sandbox", 403, func(t *testing.T, w *world) *http.Request {
			tok := w.issuer.Mint(t, vercelsandboxtest.Token{Audience: []string{base + "/e/" + host}, ProjectID: project,
				SandboxName: "someone-else"})
			return forwarded("GET", host, "/", tok, nil)
		}},
		{"a forwarded host mismatch", 400, func(t *testing.T, w *world) *http.Request {
			r := forwarded("GET", host, "/", w.token(t, host), nil)
			r.Header.Set("Vercel-Forwarded-Host", "evil.example.com")
			return r
		}},
		{"plain http", 400, func(t *testing.T, w *world) *http.Request {
			r := forwarded("GET", host, "/", w.token(t, host), nil)
			r.Header.Set("Vercel-Forwarded-Scheme", "http")
			return r
		}},
		{"another port", 400, func(t *testing.T, w *world) *http.Request {
			r := forwarded("GET", host, "/", w.token(t, host), nil)
			r.Header.Set("Vercel-Forwarded-Port", "8443")
			return r
		}},
		{"a path mismatch", 400, func(t *testing.T, w *world) *http.Request {
			r := forwarded("GET", host, "/a", w.token(t, host), nil)
			r.Header.Set("Vercel-Forwarded-Path", "/b")
			return r
		}},
		{"a host not in the snapshot", 403, func(t *testing.T, w *world) *http.Request {
			return forwarded("GET", "other.example.com", "/", w.token(t, "other.example.com"), nil)
		}},
		{"an oversized body", 413, func(t *testing.T, w *world) *http.Request {
			return forwarded("POST", host, "/", w.token(t, host), strings.NewReader(strings.Repeat("x", proxy.MaxRequestBody+1)))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t, nil)
			rec := serve(w.handler, c.build(t, w))
			if rec.Code != c.status || len(w.upstream.requests) != 0 {
				t.Errorf("got %d with %d origin requests; want %d and none", rec.Code, len(w.upstream.requests), c.status)
			}
		})
	}
}

// TestAnAuthorizationFailureRefuses: an ended runner, a missing snapshot or a
// database failure all refuse, and nothing is forwarded on doubt.
func TestAnAuthorizationFailureRefuses(t *testing.T) {
	for _, err := range []error{application.ErrEgressRunnerNotLive, application.ErrEgressSnapshotMissing, errors.New("db down")} {
		w := newWorld(t, nil)
		w.authorizer.err = err
		if rec := serve(w.handler, forwarded("GET", host, "/", w.token(t, host), nil)); rec.Code != 403 || len(w.upstream.requests) != 0 {
			t.Errorf("%v: got %d with %d origin requests", err, rec.Code, len(w.upstream.requests))
		}
	}
}

// TestARedirectReturnsToTheSandbox: the proxy follows nothing; the sandbox's
// own policy decides whether the target is reachable.
func TestARedirectReturnsToTheSandbox(t *testing.T) {
	w := newWorld(t, nil)
	w.upstream.respond = func(*http.Request) (*http.Response, error) {
		h := http.Header{}
		h.Set("Location", "https://internal.example/")
		return &http.Response{StatusCode: 302, Header: h, Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	rec := serve(w.handler, forwarded("GET", host, "/", w.token(t, host), nil))
	if rec.Code != 302 || rec.Header().Get("Location") != "https://internal.example/" || len(w.upstream.requests) != 1 {
		t.Errorf("got %d to %q after %d requests", rec.Code, rec.Header().Get("Location"), len(w.upstream.requests))
	}
}

// TestAForbiddenAddressIsRefusedThroughTheRealGuard: an authorized host whose
// DNS answers a private address is refused by the guarded transport, with a
// category and never the address.
func TestAForbiddenAddressIsRefusedThroughTheRealGuard(t *testing.T) {
	for _, answer := range []string{"127.0.0.1", "10.1.2.3", "169.254.169.254", "::1", "fd00::1"} {
		client := guardedhttp.NewWithResolver(func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr(answer)}, nil
		})
		w := newWorld(t, client)
		rec := serve(w.handler, forwarded("GET", host, "/", w.token(t, host), nil))
		if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), answer) {
			t.Errorf("%s: got %d %q", answer, rec.Code, rec.Body.String())
		}
		client.CloseIdleConnections()
	}
}

// TestAStalledBodyEndsTheRequest: over a real connection, a body that stops
// arriving ends the request instead of holding it, and nothing is forwarded.
func TestAStalledBodyEndsTheRequest(t *testing.T) {
	w := newWorld(t, nil)
	server := httptest.NewServer(w.handler)
	t.Cleanup(server.Close)
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var head strings.Builder
	head.WriteString("POST /e/" + host + "/ HTTP/1.1\r\nHost: proxy\r\nContent-Length: 1000\r\n")
	for name, values := range forwarded("POST", host, "/", w.token(t, host), nil).Header {
		head.WriteString(name + ": " + values[0] + "\r\n")
	}
	head.WriteString("\r\n" + strings.Repeat("x", 10))
	if _, err := conn.Write([]byte(head.String())); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _, _ = io.ReadAll(conn); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a stalled body held the request")
	}
	if len(w.upstream.requests) != 0 {
		t.Error("a body that never arrived was forwarded")
	}
}
