// Package guardedhttp implements ADR-017's public-destination HTTP boundary.
// It does not authorize a hostname: callers must first check the live runner's
// immutable host snapshot. It prevents that hostname from reaching forbidden IPs.
package guardedhttp

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// ErrDestination is a safe error category for an invalid or forbidden target.
var ErrDestination = errors.New("egress: destination refused")

// ErrResolution means DNS failed, timed out, or supplied no usable addresses.
var ErrResolution = errors.New("egress: resolution failed")

// ErrConnect means all validated connection candidates failed.
var ErrConnect = errors.New("egress: connection failed")

// ErrUpstream means TLS, response headers or HTTP exchange failed.
var ErrUpstream = errors.New("egress: upstream request failed")

// Client fetches HTTPS without redirects, environment proxies or tunnels.
// Its transport and test seams are private so callers cannot bypass the guard.
type Client struct {
	client    *http.Client
	transport *http.Transport
}

// New returns a client with bounded DNS, connect, TLS and header waits. Response
// bodies stream under the caller's context; callers must close them and enforce
// service concurrency/body limits. This is not a complete forwarding service.
func New() *Client {
	resolver := &net.Resolver{PreferGo: true}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return newClient(resolver.LookupNetIP, dialer.DialContext, nil)
}

// NewWithResolver is New with an injected resolver, for callers' tests that
// must exercise the real address guard: whatever the resolver answers is still
// validated in full, and dialing still uses only validated numeric addresses.
// There is deliberately no exported dialer or TLS seam.
func NewWithResolver(lookup func(ctx context.Context, network, host string) ([]netip.Addr, error)) *Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return newClient(lookup, dialer.DialContext, nil)
}

type lookupFunc func(context.Context, string, string) ([]netip.Addr, error)
type dialFunc func(context.Context, string, string) (net.Conn, error)

// newClient constructs the production guard with private resolver/dial/TLS-root test seams.
func newClient(lookup lookupFunc, dial dialFunc, roots *tls.Config) *Client {
	guard := func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || port != "443" || !validHostname(host) {
			return nil, ErrDestination
		}
		bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		ips, err := lookup(bounded, "ip", host)
		if bounded.Err() != nil {
			return nil, bounded.Err()
		}
		if err != nil || len(ips) == 0 {
			return nil, ErrResolution
		}
		// Validate the entire answer before dialing any candidate. A mixed public /
		// forbidden answer is refused, even if the public candidate would work.
		for _, ip := range ips {
			if !publicAddress(ip) {
				return nil, ErrDestination
			}
		}
		for _, ip := range ips {
			if bounded.Err() != nil {
				break
			}
			conn, err := dial(bounded, "tcp", net.JoinHostPort(ip.Unmap().String(), "443"))
			if err == nil {
				return conn, nil
			}
		}
		if bounded.Err() != nil {
			return nil, bounded.Err()
		}
		return nil, ErrConnect
	}
	if roots == nil {
		roots = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	transport := &http.Transport{
		Proxy: nil, DialContext: guard, TLSHandshakeTimeout: 10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second, IdleConnTimeout: 30 * time.Second,
		MaxIdleConns: 32, MaxIdleConnsPerHost: 2, MaxConnsPerHost: 8,
		MaxResponseHeaderBytes: 1 << 20, TLSClientConfig: roots,
	}
	return &Client{transport: transport, client: &http.Client{Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// Do fetches a previously authorized origin. Only origin-form HTTPS requests
// on 443 with a matching Host are supported. No CONNECT, TRACE or upgrade.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil || req.URL.Scheme != "https" || req.URL.User != nil || req.URL.Fragment != "" ||
		!validHostname(req.URL.Hostname()) || (req.URL.Host != req.URL.Hostname() && req.URL.Host != req.URL.Hostname()+":443") ||
		(req.Host != "" && req.Host != req.URL.Host) || req.Header.Get("Upgrade") != "" || hasUpgrade(req.Header.Values("Connection")) {
		return nil, ErrDestination
	}
	switch req.Method {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS":
	default:
		return nil, ErrDestination
	}
	resp, err := c.client.Do(req)
	if err != nil {
		// net/http wraps errors in url.Error, which includes the full URL/query.
		// Return only stable categories and cancellation, never that secret-bearing URL.
		for _, category := range []error{ErrDestination, ErrResolution, ErrConnect, context.Canceled, context.DeadlineExceeded} {
			if errors.Is(err, category) {
				return nil, category
			}
		}
		return nil, ErrUpstream
	}
	if resp.StatusCode == http.StatusSwitchingProtocols {
		_ = resp.Body.Close()
		return nil, ErrDestination
	}
	return resp, nil
}

// CloseIdleConnections releases pooled connections, without affecting active
// responses. Every subsequently opened connection repeats resolution and checks.
func (c *Client) CloseIdleConnections() { c.transport.CloseIdleConnections() }

// hasUpgrade detects upgrade tokens across all Connection header values.
func hasUpgrade(values []string) bool {
	for _, value := range values {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				return true
			}
		}
	}
	return false
}

// validHostname rejects noncanonical names, raw IPs and local/internal suffixes before DNS.
func validHostname(host string) bool {
	if len(host) > 253 || !strings.Contains(host, ".") || strings.ToLower(host) != host {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return false
	}
	for _, suffix := range []string{".localhost", ".local", ".internal", ".home.arpa"} {
		if strings.HasSuffix(host, suffix) {
			return false
		}
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, ch := range label {
			if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') && ch != '-' {
				return false
			}
		}
	}
	return true
}
