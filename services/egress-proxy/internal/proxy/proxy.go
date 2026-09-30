// Package proxy is the egress proxy's request path (Unit M5.4d.3a, ADR-017):
// authenticate, authorize against the runner's own snapshot, then forward
// through the guarded transport.
//
// Vercel's firewall sends every request a runner makes to a workspace-added
// host here — an origin-form request to <public base>/e/<host>/<original path>,
// the destination in Vercel-Forwarded-* headers, and a Vercel-signed OIDC
// token naming the sandbox. Every header is a claim; only what the token proves
// is trusted. Nothing about the request's content is logged.
package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/adapters/guardedhttp"
	"github.com/jigmetnamgyal/weave/internal/adapters/vercelsandbox"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// Verifier checks a Vercel sandbox OIDC token for one route.
type Verifier interface {
	Verify(ctx context.Context, raw, audience string) (vercelsandbox.SandboxToken, error)
}

// Authorizer decides whether a runner may reach a host: live, forwarding, and
// the exact host in its own immutable snapshot.
type Authorizer interface {
	Authorize(ctx context.Context, runnerID uuid.UUID, host string) (application.RegistryRunner, error)
}

// Upstream fetches from an authorized origin. In production it is the guarded
// transport, which refuses forbidden addresses on every new connection.
type Upstream interface {
	Do(*http.Request) (*http.Response, error)
}

// Headers Vercel adds.
const (
	headerToken  = "Vercel-Sandbox-Oidc-Token"
	headerHost   = "Vercel-Forwarded-Host"
	headerPath   = "Vercel-Forwarded-Path"
	headerScheme = "Vercel-Forwarded-Scheme"
	headerPort   = "Vercel-Forwarded-Port"
)

// DefaultBodyReadTimeout bounds reading a request's body: it is read whole
// before anything is forwarded, and the header timeout does not cover it.
const DefaultBodyReadTimeout = 60 * time.Second

// DefaultIdleTimeout bounds how long a response stream may make no progress.
// It limits stalls, not size: every chunk read from the origin, and every
// chunk written to the sandbox, resets it (review of PR #29).
const DefaultIdleTimeout = 60 * time.Second

// MaxRequestBody bounds a forwarded request's body.
const MaxRequestBody = 8 << 20

// Config sets up the handler.
type Config struct {
	PublicBase string
	Verifier   Verifier
	Authorizer Authorizer
	// Upstream is nil for the guarded transport.
	Upstream        Upstream
	Logger          *slog.Logger
	BodyReadTimeout time.Duration
	// IdleTimeout is DefaultIdleTimeout when zero.
	IdleTimeout time.Duration
}

// Handler serves forwarded egress requests.
type Handler struct {
	base       string
	verifier   Verifier
	authorizer Authorizer
	upstream   Upstream
	logger     *slog.Logger
	bodyRead   time.Duration
	idle       time.Duration
}

// New builds the handler.
func New(cfg Config) *Handler {
	upstream := cfg.Upstream
	if upstream == nil {
		upstream = guardedhttp.New()
	}
	bodyRead := cfg.BodyReadTimeout
	if bodyRead == 0 {
		bodyRead = DefaultBodyReadTimeout
	}
	idle := cfg.IdleTimeout
	if idle == 0 {
		idle = DefaultIdleTimeout
	}
	return &Handler{base: strings.TrimSuffix(cfg.PublicBase, "/"), verifier: cfg.Verifier, authorizer: cfg.Authorizer,
		upstream: upstream, logger: cfg.Logger, bodyRead: bodyRead, idle: idle}
}

// ServeHTTP refuses at the first failed step, and forwards only after all of
// them: route, token, forwarded headers, runner and snapshot, bounded body.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// 1. The route names the host, compared in its encoded form.
	rest, ok := strings.CutPrefix(r.URL.EscapedPath(), "/e/")
	host, routePath, _ := strings.Cut(rest, "/")
	if !ok || host == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	canonical, err := domain.ValidateEgressHostname(host)
	if err != nil || canonical != host {
		h.refuse(w, r, http.StatusNotFound, "not a destination this proxy serves", "", nil)
		return
	}

	// 2. Authenticate before anything else is read or touched. The audience
	// is this route, so a token issued for one host is refused for another.
	token, err := h.verifier.Verify(r.Context(), r.Header.Get(headerToken), h.base+"/e/"+host)
	switch {
	case errors.Is(err, application.ErrVerifierUnavailable):
		h.refuse(w, r, http.StatusServiceUnavailable, "the sandbox token cannot be verified now", host, err)
		return
	case err != nil:
		h.refuse(w, r, http.StatusUnauthorized, "a valid sandbox token is required", host, err)
		return
	}
	runnerID, _, err := vercelsandbox.RunnerFromSandboxName(token.SandboxName)
	if err != nil {
		h.refuse(w, r, http.StatusForbidden, "not a runner sandbox", host, err)
		return
	}

	// 3. Every Vercel-Forwarded-* header is a claim, checked against the route.
	forwardedPath := r.Header.Get(headerPath)
	pathOnly, _, _ := strings.Cut(forwardedPath, "?")
	switch {
	case !strings.EqualFold(r.Header.Get(headerHost), host):
		h.refuse(w, r, http.StatusBadRequest, "the forwarded host does not match the route", host, nil)
		return
	case r.Header.Get(headerScheme) != "https":
		h.refuse(w, r, http.StatusBadRequest, "only https is forwarded", host, nil)
		return
	case r.Header.Get(headerPort) != "" && r.Header.Get(headerPort) != "443":
		h.refuse(w, r, http.StatusBadRequest, "only port 443 is forwarded", host, nil)
		return
	case !strings.HasPrefix(forwardedPath, "/") || pathOnly != "/"+routePath:
		h.refuse(w, r, http.StatusBadRequest, "the forwarded path does not match the request", host, nil)
		return
	}

	// 4–5. The runner, and the host in its own snapshot. Any failure —
	// including a database error — refuses: nothing is forwarded on doubt.
	runner, err := h.authorizer.Authorize(r.Context(), runnerID, host)
	if err != nil {
		h.refuse(w, r, http.StatusForbidden, "this runner may not reach this host", host, err)
		return
	}

	// 6. The body, bounded, before anything is sent. The read deadline stays
	// set on failure so draining an incomplete body cannot stall.
	control := http.NewResponseController(w)
	_ = control.SetReadDeadline(time.Now().Add(h.bodyRead))
	body, tooLarge, err := readBody(r)
	if err == nil && !tooLarge {
		_ = control.SetReadDeadline(time.Time{})
	}
	switch {
	case tooLarge:
		h.refuse(w, r, http.StatusRequestEntityTooLarge, "the request body is larger than the proxy forwards", host, nil)
		return
	case err != nil:
		h.refuse(w, r, http.StatusBadRequest, "the request body could not be read", host, err)
		return
	}

	// 7. Forward through the guarded transport.
	status, category := h.forward(w, r, host, forwardedPath, body)
	h.logger.InfoContext(r.Context(), "egress request",
		slog.String("session_id", runner.SessionID.String()), slog.String("runner_id", runnerID.String()),
		slog.String("host", host), slog.String("method", r.Method), slog.Int("status", status),
		slog.String("outcome", category), slog.Duration("duration", time.Since(start)))
}

// forward fetches from the origin and streams the answer back. A refused or
// failed upstream answers with a stable category, never an address.
func (h *Handler) forward(w http.ResponseWriter, r *http.Request, host, path string, body []byte) (int, string) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	// Cancelled when the stream stalls, which ends the origin's read.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	upstream, err := http.NewRequestWithContext(ctx, r.Method, "https://"+host+path, reader)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return http.StatusBadRequest, "bad_request"
	}
	copyHeaders(upstream.Header, r.Header)
	upstream.ContentLength = int64(len(body))

	response, err := h.upstream.Do(upstream)
	if err != nil {
		status, category, message := http.StatusBadGateway, "upstream_failed", "the destination could not be reached"
		switch {
		case errors.Is(err, guardedhttp.ErrDestination):
			category, message = "destination_refused", "the destination is not a permitted public address"
		case errors.Is(err, guardedhttp.ErrResolution):
			category = "resolution_failed"
		case errors.Is(err, context.DeadlineExceeded):
			status, category = http.StatusGatewayTimeout, "timeout"
		}
		http.Error(w, message, status)
		return status, category
	}
	defer func() { _ = response.Body.Close() }()
	copyHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	if stalled := h.stream(w, response.Body, cancel); stalled {
		return response.StatusCode, "stalled"
	}
	return response.StatusCode, "forwarded"
}

// stream copies the response while it makes progress. No total deadline: a
// large download takes as long as it takes. But a read that yields nothing
// for the idle timeout cancels the origin request, and each write to the
// sandbox gets the same bound, so a stalled peer on either side cannot hold
// the request open.
func (h *Handler) stream(w http.ResponseWriter, body io.Reader, cancel context.CancelFunc) (stalled bool) {
	control := http.NewResponseController(w)
	idle := time.AfterFunc(h.idle, cancel)
	defer idle.Stop()
	buf := make([]byte, 32<<10)
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			idle.Reset(h.idle)
			_ = control.SetWriteDeadline(time.Now().Add(h.idle))
			if _, err := w.Write(buf[:n]); err != nil {
				return true
			}
			_ = control.Flush()
		}
		if readErr != nil {
			return !errors.Is(readErr, io.EOF)
		}
	}
}

// readBody reads the request body whole, refusing one over MaxRequestBody.
func readBody(r *http.Request) (body []byte, tooLarge bool, err error) {
	if r.Body == nil || r.ContentLength == 0 {
		return nil, false, nil
	}
	if r.ContentLength > MaxRequestBody {
		return nil, true, nil
	}
	body, err = io.ReadAll(io.LimitReader(r.Body, MaxRequestBody+1))
	if err != nil {
		return nil, false, err
	}
	if len(body) > MaxRequestBody {
		return nil, true, nil
	}
	return body, false, nil
}

// hopByHop are the headers that describe one connection, never the message.
var hopByHop = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true, "Proxy-Authorization": true,
	"Proxy-Connection": true, "Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true,
}

// addedOnTheWay are headers the path to this proxy added — Vercel's forwarding
// and token, a tunnel's or load balancer's — none of which may reach an origin.
var addedOnTheWay = []string{"Vercel-", "Cf-", "Cdn-Loop", "X-Forwarded-", "X-Real-Ip", "Forwarded", "Via"}

// copyHeaders copies a message's headers, dropping hop-by-hop headers and
// everything added on the way, in both directions.
func copyHeaders(dst, src http.Header) {
	named := map[string]bool{}
	for _, value := range src.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			named[http.CanonicalHeaderKey(strings.TrimSpace(name))] = true
		}
	}
	for name, values := range src {
		canonical := http.CanonicalHeaderKey(name)
		if hopByHop[canonical] || named[canonical] {
			continue
		}
		if canonical == "Host" || hasAnyPrefix(canonical, addedOnTheWay) {
			continue
		}
		for _, value := range values {
			dst.Add(canonical, value)
		}
	}
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// refuse answers a refusal and logs why — as a stable category, never the
// cause's text. Error messages can carry request-derived content (a sandbox
// name from the token, for one), and nothing about a request's content is
// logged (review of PR #29).
func (h *Handler) refuse(w http.ResponseWriter, r *http.Request, status int, message, host string, cause error) {
	attrs := []any{slog.Int("status", status), slog.String("host", host), slog.String("reason", message)}
	if cause != nil {
		attrs = append(attrs, slog.String("cause", causeCategory(cause)))
	}
	h.logger.WarnContext(r.Context(), "egress request refused", attrs...)
	http.Error(w, message, status)
}

// causeCategory names a refusal's cause without repeating its text.
func causeCategory(err error) string {
	switch {
	case errors.Is(err, application.ErrVerifierUnavailable):
		return "verifier_unavailable"
	case errors.Is(err, application.ErrEgressHostNotAuthorized):
		return "host_not_in_snapshot"
	case errors.Is(err, application.ErrEgressRunnerNotLive):
		return "runner_not_live"
	case errors.Is(err, application.ErrEgressSnapshotMissing):
		return "snapshot_missing"
	case errors.Is(err, domain.ErrRunnerNotFound):
		return "runner_not_found"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "cancelled"
	}
	return "other"
}
