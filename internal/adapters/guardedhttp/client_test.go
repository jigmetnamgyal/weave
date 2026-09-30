package guardedhttp

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestAddressClassification checks supported origins, forbidden classes and range boundaries.
func TestAddressClassification(t *testing.T) {
	for _, value := range []string{"8.8.8.8", "1.1.1.1", "140.82.112.3", "2001:4860:4860::8888", "2606:4700:4700::1111", "::ffff:8.8.8.8"} {
		if !publicAddress(netip.MustParseAddr(value)) {
			t.Errorf("public address refused: %s", value)
		}
	}
	for _, value := range []string{"0.1.2.3", "10.1.2.3", "100.64.1.1", "127.0.0.1", "169.254.169.254", "172.16.1.1", "172.31.255.255", "192.0.0.9", "192.0.2.1", "192.31.196.1", "192.52.193.1", "192.88.99.1", "192.168.1.1", "192.175.48.1", "198.18.1.1", "198.51.100.1", "203.0.113.1", "224.0.0.1", "255.255.255.255", "168.63.129.16", "::", "::1", "::ffff:127.0.0.1", "::ffff:10.1.2.3", "64:ff9b::a00:1", "64:ff9b:1::1", "100::1", "100:0:0:1::1", "2001::1", "2001:2::1", "2001:3::1", "2001:db8::1", "2002:a00:1::1", "2620:4f:8000::1", "3fff::1", "5f00::1", "fc00::1", "fe80::1", "ff02::1", "2001:4860::1%eth0"} {
		if publicAddress(netip.MustParseAddr(value)) {
			t.Errorf("forbidden address accepted: %s", value)
		}
	}
	if publicAddress(netip.Addr{}) {
		t.Error("invalid address accepted")
	}
	// Boundary checks independent of the range table itself.
	for _, c := range []struct {
		ip   string
		want bool
	}{{"100.63.255.255", true}, {"100.128.0.0", true}, {"172.15.255.255", true}, {"172.32.0.0", true}, {"192.167.255.255", true}, {"192.169.0.0", true}, {"198.17.255.255", true}, {"198.20.0.0", true}, {"223.255.255.255", true}, {"2001:1ff::1", false}, {"2001:200::1", true}, {"3ffe:ffff::1", true}, {"3fff:fff::1", false}, {"3fff:1000::1", true}} {
		if got := publicAddress(netip.MustParseAddr(c.ip)); got != c.want {
			t.Errorf("boundary %s: %v want %v", c.ip, got, c.want)
		}
	}
}

// fixture keeps the production address classifier enabled. Only the private
// dial seam maps an already validated public numeric target to a local TLS
// server. No allow-private flag or insecure TLS is present in production.
func fixture(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *tls.Config) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "public.example"}, DNSNames: []string{"public.example"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}

// request builds a test request while preserving the supplied URL for validation.
func request(t *testing.T, raw string) *http.Request {
	t.Helper()
	r, e := http.NewRequest("GET", raw, nil)
	if e != nil {
		t.Fatal(e)
	}
	// NewRequest removes an empty port. Preserve the supplied URL to exercise
	// rejection even for callers constructing requests without NewRequest.
	r.URL, e = url.Parse(raw)
	if e != nil {
		t.Fatal(e)
	}
	return r
}

// TestNumericDialAndOriginalTLSHost verifies one lookup, numeric dialing and original Host/SNI.
func TestNumericDialAndOriginalTLSHost(t *testing.T) {
	var mu sync.Mutex
	var names, targets []string
	server, roots := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "public.example" || r.TLS.ServerName != "public.example" {
			t.Errorf("origin identity changed: Host=%s SNI=%s", r.Host, r.TLS.ServerName)
		}
		_, _ = io.WriteString(w, "fixture-marker")
	})
	c := newClient(func(_ context.Context, network, host string) ([]netip.Addr, error) {
		mu.Lock()
		defer mu.Unlock()
		names = append(names, network+":"+host)
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}, func(ctx context.Context, network, address string) (net.Conn, error) {
		mu.Lock()
		targets = append(targets, address)
		mu.Unlock()
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}, roots)
	t.Cleanup(c.CloseIdleConnections)
	resp, err := c.Do(request(t, "https://public.example/"))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || string(body) != "fixture-marker" {
		t.Fatalf("fixture: %q %v", body, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(names) != 1 || names[0] != "ip:public.example" || len(targets) != 1 || targets[0] != "8.8.8.8:443" {
		t.Fatalf("lookup=%v dial=%v", names, targets)
	}
}

// TestForbiddenAndMixedAnswersNeverDial proves all answers are validated before any connection attempt.
func TestForbiddenAndMixedAnswersNeverDial(t *testing.T) {
	for _, answers := range [][]string{{"127.0.0.1"}, {"10.0.0.1"}, {"169.254.169.254"}, {"::1"}, {"::ffff:10.0.0.1"}, {"fc00::1"}, {"2002:a00:1::1"}, {"8.8.8.8", "10.0.0.1"}, {"2001:4860::1", "fe80::1"}} {
		t.Run(strings.Join(answers, ","), func(t *testing.T) {
			ips := make([]netip.Addr, 0, len(answers))
			for _, s := range answers {
				ips = append(ips, netip.MustParseAddr(s))
			}
			c := newClient(func(context.Context, string, string) ([]netip.Addr, error) { return ips, nil }, func(context.Context, string, string) (net.Conn, error) {
				t.Error("dial reached for forbidden answer")
				return nil, errors.New("unexpected dial")
			}, nil)
			_, err := c.Do(request(t, "https://public.example/"))
			if !errors.Is(err, ErrDestination) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

// TestRebindingIsCheckedOnTheNextConnection refuses a changed private answer before a second dial.
func TestRebindingIsCheckedOnTheNextConnection(t *testing.T) {
	server, roots := fixture(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "marker") })
	var mu sync.Mutex
	lookups, dials := 0, 0
	c := newClient(func(context.Context, string, string) ([]netip.Addr, error) {
		mu.Lock()
		defer mu.Unlock()
		lookups++
		if lookups == 1 {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}, func(ctx context.Context, network, address string) (net.Conn, error) {
		mu.Lock()
		dials++
		mu.Unlock()
		if address != "8.8.8.8:443" {
			t.Errorf("hostname or forbidden redial: %s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}, roots)
	t.Cleanup(c.CloseIdleConnections)
	first := request(t, "https://public.example/")
	first.Close = true
	resp, err := c.Do(first)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	_, err = c.Do(request(t, "https://public.example/"))
	if !errors.Is(err, ErrDestination) {
		t.Fatalf("rebound origin accepted: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if lookups != 2 || dials != 1 {
		t.Fatalf("lookup=%d dial=%d", lookups, dials)
	}
}

// TestRedirectIsReturnedWithoutFollowing ensures redirect destinations never trigger a lookup.
func TestRedirectIsReturnedWithoutFollowing(t *testing.T) {
	server, roots := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://private.example/secret")
		w.WriteHeader(http.StatusFound)
	})
	lookups := 0
	c := newClient(func(context.Context, string, string) ([]netip.Addr, error) {
		lookups++
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}, func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}, roots)
	defer c.CloseIdleConnections()
	resp, err := c.Do(request(t, "https://public.example/"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound || lookups != 1 {
		t.Fatalf("redirect followed: status=%d lookups=%d", resp.StatusCode, lookups)
	}
}

// TestTLSVerificationCannotBeSkipped rejects untrusted certificates and mismatched origin names.
func TestTLSVerificationCannotBeSkipped(t *testing.T) {
	server, roots := fixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("HTTP reached despite bad TLS identity") })
	for _, c := range []*Client{
		newClient(func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		}, func(ctx context.Context, n, a string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, n, server.Listener.Addr().String())
		}, nil),
		newClient(func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		}, func(ctx context.Context, n, a string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, n, server.Listener.Addr().String())
		}, roots),
	} {
		_, err := c.Do(request(t, "https://wrong.example/"))
		c.CloseIdleConnections()
		if !errors.Is(err, ErrUpstream) {
			t.Fatalf("untrusted or wrong-host TLS accepted: %v", err)
		}
	}
}

// TestInvalidRequestsNeverResolve rejects unsafe origins, methods and tunnels before DNS.
func TestInvalidRequestsNeverResolve(t *testing.T) {
	c := newClient(func(context.Context, string, string) ([]netip.Addr, error) {
		t.Error("invalid request reached DNS")
		return nil, nil
	}, nil, nil)
	for _, raw := range []string{"http://public.example/", "https://public.example:444/", "https://public.example:/", "https://u:p@public.example/", "https://public.example/#fragment", "https://127.0.0.1/", "https://[::1]/", "https://public.example./", "https://PUBLIC.example/", "https://foo.local/", "https://foo.internal/", "https://foo.home.arpa/"} {
		if _, err := c.Do(request(t, raw)); !errors.Is(err, ErrDestination) {
			t.Errorf("%s: %v", raw, err)
		}
	}
	for _, method := range []string{"CONNECT", "TRACE"} {
		r := request(t, "https://public.example/")
		r.Method = method
		if _, err := c.Do(r); !errors.Is(err, ErrDestination) {
			t.Errorf("%s: %v", method, err)
		}
	}
	for _, change := range []func(*http.Request){func(r *http.Request) { r.Host = "other.example" }, func(r *http.Request) { r.Header.Set("Upgrade", "websocket") }, func(r *http.Request) { r.Header.Add("Connection", "keep-alive, UpGrAdE") }} {
		r := request(t, "https://public.example/")
		change(r)
		if _, err := c.Do(r); !errors.Is(err, ErrDestination) {
			t.Errorf("unsafe request: %v", err)
		}
	}
	if _, err := c.Do(nil); !errors.Is(err, ErrDestination) {
		t.Errorf("nil request: %v", err)
	}
}

// TestResolutionFailuresAreSafeAndBounded covers empty answers, DNS errors and cancellation without leaking URLs.
func TestResolutionFailuresAreSafeAndBounded(t *testing.T) {
	for name, lookup := range map[string]lookupFunc{
		"empty": func(context.Context, string, string) ([]netip.Addr, error) { return nil, nil },
		"error": func(context.Context, string, string) ([]netip.Addr, error) {
			return nil, errors.New("resolver detail secret")
		},
		"cancellation": func(ctx context.Context, _, _ string) ([]netip.Addr, error) { <-ctx.Done(); return nil, ctx.Err() },
	} {
		t.Run(name, func(t *testing.T) {
			c := newClient(lookup, func(context.Context, string, string) (net.Conn, error) {
				t.Error("resolution failure dialed")
				return nil, nil
			}, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			r := request(t, "https://public.example/path?secret=credential")
			r = r.WithContext(ctx)
			_, err := c.Do(r)
			if name == "cancellation" && !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("cancellation not preserved: %v", err)
			}
			if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "credential") {
				t.Fatalf("unsafe error: %v", err)
			}
		})
	}
}

// TestEnvironmentProxyIsNotUsed ensures proxy environment variables cannot bypass numeric dialing.
func TestEnvironmentProxyIsNotUsed(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "http://127.0.0.1:1")
	c := New()
	defer c.CloseIdleConnections()
	if c.transport.Proxy != nil {
		t.Fatal("environment proxy configured")
	}
	if c.transport.DialTLSContext != nil || c.transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("alternate TLS bypass")
	}
}

// TestFallbackDialsOnlyValidatedNumericCandidates checks fallback stays in the validated answer set.
func TestFallbackDialsOnlyValidatedNumericCandidates(t *testing.T) {
	server, roots := fixture(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "marker") })
	var mu sync.Mutex
	var targets []string
	c := newClient(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("::ffff:8.8.8.8")}, nil
	}, func(ctx context.Context, n, a string) (net.Conn, error) {
		mu.Lock()
		targets = append(targets, a)
		mu.Unlock()
		if a == "1.1.1.1:443" {
			return nil, errors.New("unreachable")
		}
		return (&net.Dialer{}).DialContext(ctx, n, server.Listener.Addr().String())
	}, roots)
	defer c.CloseIdleConnections()
	resp, err := c.Do(request(t, "https://public.example:443/"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(targets, ",") != "1.1.1.1:443,8.8.8.8:443" {
		t.Fatalf("unsafe fallback: %v", targets)
	}
}

// TestHeaderTimeoutAndUnsolicitedUpgrade bounds response headers and refuses unsolicited protocol switching.
func TestHeaderTimeoutAndUnsolicitedUpgrade(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		t.Run(map[bool]string{false: "slow headers", true: "unsolicited upgrade"}[upgrade], func(t *testing.T) {
			server, roots := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if upgrade {
					w.WriteHeader(101)
					return
				}
				select {
				case <-r.Context().Done():
				case <-time.After(time.Second):
				}
			})
			c := newClient(func(context.Context, string, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
			}, func(ctx context.Context, n, a string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, n, server.Listener.Addr().String())
			}, roots)
			c.transport.ResponseHeaderTimeout = 50 * time.Millisecond
			defer c.CloseIdleConnections()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, err := c.Do(request(t, "https://public.example/?secret=token").WithContext(ctx))
			want := context.DeadlineExceeded
			if upgrade {
				want = ErrDestination
			}
			if !errors.Is(err, want) {
				t.Fatalf("unbounded/upgrade response: %v, want %v", err, want)
			}
		})
	}
}
