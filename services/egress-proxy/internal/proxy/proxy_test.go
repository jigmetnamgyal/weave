package proxy_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
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

// TestAStalledResponseIsCutOff is review feedback on PR #29: an origin that
// sent headers and then stopped held the request open indefinitely. Now a
// stream that makes no progress for the idle timeout is ended, while what
// already arrived was passed through.
func TestAStalledResponseIsCutOff(t *testing.T) {
	w := newWorld(t, nil)
	w.handler = proxy.New(proxy.Config{PublicBase: base, Verifier: w.verifier, Authorizer: w.authorizer, Upstream: w.upstream,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), IdleTimeout: 200 * time.Millisecond})
	w.upstream.respond = func(r *http.Request) (*http.Response, error) {
		reader, writer := io.Pipe()
		go func() {
			_, _ = writer.Write([]byte("partial "))
			<-r.Context().Done() // stall until the proxy gives up
			_ = writer.CloseWithError(r.Context().Err())
		}()
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: reader}, nil
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- serve(w.handler, forwarded("GET", host, "/big", w.token(t, host), nil)) }()
	select {
	case rec := <-done:
		if !strings.HasPrefix(rec.Body.String(), "partial ") {
			t.Errorf("body %q; want what arrived before the stall", rec.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stalled response held the request for 5s")
	}
}

// TestASlowButSteadyResponseIsNotCutOff: the bound is on stalls, not on size
// or total time — a stream that keeps arriving outlives the idle timeout.
func TestASlowButSteadyResponseIsNotCutOff(t *testing.T) {
	w := newWorld(t, nil)
	w.handler = proxy.New(proxy.Config{PublicBase: base, Verifier: w.verifier, Authorizer: w.authorizer, Upstream: w.upstream,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), IdleTimeout: 200 * time.Millisecond})
	w.upstream.respond = func(*http.Request) (*http.Response, error) {
		reader, writer := io.Pipe()
		go func() {
			for i := 0; i < 8; i++ { // 8 chunks, 100ms apart: 800ms total, 4x the idle bound
				_, _ = writer.Write([]byte("x"))
				time.Sleep(100 * time.Millisecond)
			}
			_ = writer.Close()
		}()
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: reader}, nil
	}
	rec := serve(w.handler, forwarded("GET", host, "/steady", w.token(t, host), nil))
	if rec.Body.String() != "xxxxxxxx" {
		t.Errorf("body %q; a steady stream was cut off", rec.Body.String())
	}
}

// TestRefusalsLogNoRequestContent is review feedback on PR #29: a refusal
// logged its cause verbatim, and the cause of a non-runner sandbox carries the
// sandbox name from the token. Refusals now log a category.
func TestRefusalsLogNoRequestContent(t *testing.T) {
	w := newWorld(t, nil)
	var logs strings.Builder
	var mu sync.Mutex
	w.handler = proxy.New(proxy.Config{PublicBase: base, Verifier: w.verifier, Authorizer: w.authorizer, Upstream: w.upstream,
		Logger: slog.New(slog.NewTextHandler(lockedWriter{&mu, &logs}, nil))})
	marker := "sandbox-name-marker-7f3a"
	tok := w.issuer.Mint(t, vercelsandboxtest.Token{Audience: []string{base + "/e/" + host}, ProjectID: project, SandboxName: marker})
	if rec := serve(w.handler, forwarded("GET", host, "/secret-path?token=q-marker", tok, nil)); rec.Code != http.StatusForbidden {
		t.Fatalf("got %d", rec.Code)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, leaked := range []string{marker, "secret-path", "q-marker"} {
		if strings.Contains(logs.String(), leaked) {
			t.Errorf("the log carries request content %q:\n%s", leaked, logs.String())
		}
	}
	if !strings.Contains(logs.String(), "egress request refused") {
		t.Errorf("the refusal was not logged at all:\n%s", logs.String())
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *strings.Builder
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// TestAReusedConnectionOutlivesAStreamsWriteDeadline follows review feedback on
// PR #29: the per-chunk write deadline is set on the connection, so a stale one
// could fail the next response on a reused keep-alive connection. Go's HTTP/1
// server clears it after each request, and the proxy clears it too; this pins
// the outcome: two requests, one connection, a pause longer than the idle
// bound between them, and the second is still answered.
func TestAReusedConnectionOutlivesAStreamsWriteDeadline(t *testing.T) {
	w := newWorld(t, nil)
	w.handler = proxy.New(proxy.Config{PublicBase: base, Verifier: w.verifier, Authorizer: w.authorizer, Upstream: w.upstream,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), IdleTimeout: 150 * time.Millisecond})
	server := httptest.NewServer(w.handler)
	t.Cleanup(server.Close)
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 1}}
	t.Cleanup(client.CloseIdleConnections)

	do := func(path string) (int, bool) {
		t.Helper()
		r := forwarded("GET", host, path, w.token(t, host), nil)
		req, err := http.NewRequest("GET", server.URL+"/e/"+host+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header = r.Header
		reused := false
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused },
		}))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode, reused
	}
	if code, _ := do("/first"); code != 200 {
		t.Fatalf("first: %d", code)
	}
	time.Sleep(400 * time.Millisecond) // well past the first stream's write deadline
	code, reused := do("/second")
	if !reused {
		t.Fatal("the second request did not reuse the connection; the test proves nothing")
	}
	if code != 200 {
		t.Errorf("second on the reused connection: %d, want 200", code)
	}
}

// slowWriter delivers each chunk slowly, as a sandbox on a congested link
// would, and can refuse flushes as one that has gone away would.
type slowWriter struct {
	*httptest.ResponseRecorder
	delay     time.Duration
	failFlush bool
}

func (s *slowWriter) Write(p []byte) (int, error) {
	time.Sleep(s.delay)
	return s.ResponseRecorder.Write(p)
}

func (s *slowWriter) FlushError() error {
	if s.failFlush {
		return errors.New("the sandbox went away")
	}
	s.Flush()
	return nil
}

// TestASlowWriteDoesNotEatTheOriginsAllowance is review feedback on PR #29:
// the idle clock was reset when a chunk was read, not when it was delivered,
// so a slow write followed by an origin pause cancelled a stream that was
// still making progress. Idle 200ms; each write takes 150ms; the origin sends
// the next chunk 250ms after the last was taken — 250ms after the read, but
// only 100ms after the write finished, so the stream is never idle for 200ms.
func TestASlowWriteDoesNotEatTheOriginsAllowance(t *testing.T) {
	w := newWorld(t, nil)
	w.handler = proxy.New(proxy.Config{PublicBase: base, Verifier: w.verifier, Authorizer: w.authorizer, Upstream: w.upstream,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), IdleTimeout: 200 * time.Millisecond})
	w.upstream.respond = func(r *http.Request) (*http.Response, error) {
		reader, writer := io.Pipe()
		go func() {
			for i := 0; i < 3; i++ {
				if _, err := writer.Write([]byte("chunk")); err != nil {
					return
				}
				select {
				case <-time.After(250 * time.Millisecond):
				case <-r.Context().Done():
					_ = writer.CloseWithError(r.Context().Err())
					return
				}
			}
			_ = writer.Close()
		}()
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: reader}, nil
	}
	rec := &slowWriter{ResponseRecorder: httptest.NewRecorder(), delay: 150 * time.Millisecond}
	w.handler.ServeHTTP(rec, forwarded("GET", host, "/slow-link", w.token(t, host), nil))
	if rec.Body.String() != "chunkchunkchunk" {
		t.Errorf("body %q; a stream making progress was cut off", rec.Body.String())
	}
}

// TestAFailedFlushIsNotLoggedAsDelivered is review feedback on PR #29: a small
// response can be written into a buffer while the sandbox has gone, and the
// failed flush was ignored — so the outcome log said "forwarded".
func TestAFailedFlushIsNotLoggedAsDelivered(t *testing.T) {
	w := newWorld(t, nil)
	var logs strings.Builder
	var mu sync.Mutex
	w.handler = proxy.New(proxy.Config{PublicBase: base, Verifier: w.verifier, Authorizer: w.authorizer, Upstream: w.upstream,
		Logger: slog.New(slog.NewTextHandler(lockedWriter{&mu, &logs}, nil))})
	rec := &slowWriter{ResponseRecorder: httptest.NewRecorder(), failFlush: true}
	w.handler.ServeHTTP(rec, forwarded("GET", host, "/", w.token(t, host), nil))
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(logs.String(), "outcome=incomplete") || strings.Contains(logs.String(), "outcome=forwarded") {
		t.Errorf("a failed delivery was logged as:\n%s", logs.String())
	}
}
