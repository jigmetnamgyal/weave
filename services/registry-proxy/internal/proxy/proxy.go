// Package proxy is the registry proxy's request path (Unit M5.4c, ADR-016):
// authenticate, record, then forward.
//
// Vercel's firewall sends every registry request from a runner sandbox here —
// an origin-form HTTP/1.1 request to <public base>/r/<host>/<original path>,
// the real destination in Vercel-Forwarded-* headers, and a Vercel-signed OIDC
// token naming the sandbox (all measured in M5.4c's spike). Every header is a
// claim; only what the token proves is trusted.
package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/adapters/vercelsandbox"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// Verifier checks a Vercel sandbox OIDC token for one route.
type Verifier interface {
	Verify(ctx context.Context, raw, audience string) (vercelsandbox.SandboxToken, error)
}

// Recorder records a request before it is forwarded.
type Recorder interface {
	Record(ctx context.Context, runnerID uuid.UUID, host, method, path string) (application.RegistryRequest, error)
}

// Headers Vercel adds.
const (
	headerToken  = "Vercel-Sandbox-Oidc-Token"
	headerHost   = "Vercel-Forwarded-Host"
	headerPath   = "Vercel-Forwarded-Path"
	headerScheme = "Vercel-Forwarded-Scheme"
	headerPort   = "Vercel-Forwarded-Port"
)

// MaxRequestBody bounds a forwarded request's body. Registry clients send
// little — an audit query, a search — and this proxy forwards nothing that
// publishes, so a large upload is refused rather than relayed.
const MaxRequestBody = 8 << 20

// Config sets up the handler.
type Config struct {
	// PublicBase is this proxy's public URL, as the runner manager puts it in
	// forwardURL — and so as Vercel puts it in each token's audience.
	PublicBase string
	Verifier   Verifier
	Recorder   Recorder
	// Upstream fetches from the registries. Nil means a client with real TLS
	// verification that never follows a redirect.
	Upstream *http.Client
	// UpstreamURL maps a registry host to its base URL; tests point it at a
	// fake. Nil means https://<host>.
	UpstreamURL func(host string) string
	Logger      *slog.Logger
}

// Handler serves forwarded registry requests.
type Handler struct {
	base     string
	verifier Verifier
	recorder Recorder
	upstream *http.Client
	origin   func(string) string
	logger   *slog.Logger
}

// New builds the handler.
func New(cfg Config) *Handler {
	upstream := cfg.Upstream
	if upstream == nil {
		upstream = &http.Client{
			Timeout: 10 * time.Minute, // a large tarball on a slow registry
			Transport: &http.Transport{
				Proxy:                 nil, // never an ambient proxy: this is the egress
				DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 60 * time.Second,
				MaxIdleConnsPerHost:   32,
				ForceAttemptHTTP2:     true,
			},
		}
	}
	// Redirects go back to the sandbox, whose own policy decides whether their
	// target is reachable. Following one here would fetch from wherever the
	// registry pointed, outside the list.
	upstream.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	origin := cfg.UpstreamURL
	if origin == nil {
		origin = func(host string) string { return "https://" + host }
	}
	return &Handler{base: strings.TrimSuffix(cfg.PublicBase, "/"), verifier: cfg.Verifier, recorder: cfg.Recorder,
		upstream: upstream, origin: origin, logger: cfg.Logger}
}

// ServeHTTP authenticates, records, then forwards — and at each step, refuses
// rather than forwarding unrecorded.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// 1. The route names the host: /r/<host>/<path>.
	rest, ok := strings.CutPrefix(r.URL.Path, "/r/")
	host, routePath, _ := strings.Cut(rest, "/")
	if !ok || host == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// 2. Authenticate before anything else is read or touched. The audience is
	// this route, so a token Vercel issued for another registry is refused.
	token, err := h.verifier.Verify(r.Context(), r.Header.Get(headerToken), h.base+"/r/"+host)
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

	// 3. The destination: every Vercel-Forwarded-* header is a claim, checked
	// against the route the token was issued for.
	forwardedPath := r.Header.Get(headerPath)
	pathOnly, _, _ := strings.Cut(forwardedPath, "?")
	switch {
	case !strings.EqualFold(r.Header.Get(headerHost), host):
		h.refuse(w, r, http.StatusBadRequest, "the forwarded host does not match the route", host, nil)
		return
	case !application.IsRegistryHost(host):
		h.refuse(w, r, http.StatusBadRequest, "not a registry this proxy serves", host, nil)
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

	// 4. Record, before a byte is fetched. A request that cannot be recorded
	// is not forwarded: the control fails closed.
	recorded, err := h.recorder.Record(r.Context(), runnerID, host, r.Method, forwardedPath)
	switch {
	case errors.Is(err, domain.ErrRunnerNotFound), errors.Is(err, application.ErrRegistryRunnerNotLive):
		h.refuse(w, r, http.StatusForbidden, "the runner is not live", host, err)
		return
	case errors.Is(err, application.ErrRegistryRequestInvalid):
		h.refuse(w, r, http.StatusBadRequest, "the request cannot be recorded", host, err)
		return
	case err != nil:
		h.refuse(w, r, http.StatusServiceUnavailable, "the request could not be recorded, so it is not forwarded", host, err)
		return
	}

	// 5. Forward.
	status, size := h.forward(w, r, host, forwardedPath)
	h.logger.InfoContext(r.Context(), "registry request",
		slog.String("session_id", recorded.SessionID.String()), slog.String("runner_id", runnerID.String()),
		slog.String("host", host), slog.String("method", recorded.Method), slog.String("path", recorded.Path),
		slog.Int("status", status), slog.Int64("bytes", size), slog.Duration("duration", time.Since(start)))
}

// forward fetches from the registry and streams the answer back.
func (h *Handler) forward(w http.ResponseWriter, r *http.Request, host, path string) (int, int64) {
	var body io.Reader
	if r.ContentLength != 0 {
		body = http.MaxBytesReader(w, r.Body, MaxRequestBody)
	}
	upstream, err := http.NewRequestWithContext(r.Context(), r.Method, h.origin(host)+path, body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return http.StatusBadRequest, 0
	}
	copyHeaders(upstream.Header, r.Header, true)
	upstream.ContentLength = r.ContentLength
	upstream.Host = host

	response, err := h.upstream.Do(upstream)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			status = http.StatusGatewayTimeout
		}
		http.Error(w, "the registry could not be reached", status)
		return status, 0
	}
	defer func() { _ = response.Body.Close() }()
	copyHeaders(w.Header(), response.Header, false)
	w.WriteHeader(response.StatusCode)
	size, _ := io.Copy(w, response.Body)
	return response.StatusCode, size
}

// hopByHop are the headers that describe one connection, never the message.
var hopByHop = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true, "Proxy-Authorization": true,
	"Proxy-Connection": true, "Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true,
}

// addedOnTheWay are prefixes of headers the path to this proxy added —
// Vercel's forwarding and token, the tunnel's or load balancer's — none of
// which belongs to the sandbox's request, and the token of which must never
// reach a registry.
var addedOnTheWay = []string{"Vercel-", "Cf-", "Cdn-Loop", "X-Forwarded-", "X-Real-Ip", "Forwarded", "Via"}

func copyHeaders(dst, src http.Header, request bool) {
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
		if request {
			if canonical == "Host" || hasAnyPrefix(canonical, addedOnTheWay) {
				continue
			}
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

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// refuse answers a refusal and logs why — never the token, never a header.
func (h *Handler) refuse(w http.ResponseWriter, r *http.Request, status int, message, host string, cause error) {
	attrs := []any{slog.Int("status", status), slog.String("host", host), slog.String("reason", message)}
	if cause != nil {
		attrs = append(attrs, slog.String("error", cause.Error()))
	}
	h.logger.WarnContext(r.Context(), "registry request refused", attrs...)
	http.Error(w, message, status)
}
