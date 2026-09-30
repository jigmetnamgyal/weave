package application

import (
	"fmt"
	"net/netip"
	"net/url"
	"strings"

	"github.com/jigmetnamgyal/weave/internal/domain"
)

// ParseEgressReservedConfig validates a deployment declaration and public service
// URLs without DNS. Errors identify configuration keys, never credential-bearing
// values. Internal names/IPs are already refused by the transport and validator.
func ParseEgressReservedConfig(env, declaration string, publicURLs map[string]string) ([]string, error) {
	if env != "development" && env != "test" && env != "staging" && env != "production" {
		return nil, fmt.Errorf("APP_ENV is invalid")
	}
	if (env == "staging" || env == "production") && strings.TrimSpace(declaration) == "" {
		return nil, fmt.Errorf("EGRESS_RESERVED_HOSTS must declare deployed service namespaces")
	}
	hosts := []string{}
	if declaration != "" {
		for _, raw := range strings.Split(declaration, ",") {
			host, err := domain.ValidateEgressHostname(strings.TrimSpace(raw))
			if err != nil {
				return nil, fmt.Errorf("EGRESS_RESERVED_HOSTS contains an invalid namespace")
			}
			hosts = append(hosts, host)
		}
	}
	for key, raw := range publicURLs {
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "ws" && u.Scheme != "wss") {
			return nil, fmt.Errorf("%s must be a service URL without credentials, query or fragment", key)
		}
		host := strings.ToLower(u.Hostname())
		if _, err := netip.ParseAddr(host); err == nil {
			continue
		}
		if host == "localhost" || !strings.Contains(host, ".") || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".home.arpa") {
			continue
		}
		canonical, err := domain.ValidateEgressHostname(host)
		if err != nil {
			return nil, fmt.Errorf("%s has an invalid hostname", key)
		}
		hosts = append(hosts, canonical)
	}
	return hosts, nil
}
