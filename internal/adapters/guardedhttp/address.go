package guardedhttp

import "net/netip"

// Conservative exclusions based on IANA special-purpose address registries,
// reviewed 2026-09-30:
// https://www.iana.org/assignments/iana-ipv4-special-registry/
// https://www.iana.org/assignments/iana-ipv6-special-registry/
// This is intentionally stricter than global-unicast: documentation, shared
// address space, transition/translation, benchmarking and reserved destinations
// are not valid origins. Explicitly public special-purpose exceptions are not
// carved out. Changes require security review and boundary tests.
var deniedIPv4 = prefixes(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
	"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
	"192.31.196.0/24", "192.52.193.0/24", "192.175.48.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24",
	"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	// Azure's platform virtual address is globally numbered but not an origin.
	"168.63.129.16/32",
)

// Only ordinary allocated IPv6 global-unicast space is supported. This
// excludes ULA, link-local, multicast, unspecified, NAT64 and mapped forms
// before unmapping. Mapped IPv4 is deliberately checked as IPv4 below.
var globalIPv6 = netip.MustParsePrefix("2000::/3")
var deniedIPv6 = prefixes(
	"2001::/23",         // IETF protocol assignments, including Teredo and benchmarking
	"2001:db8::/32",     // documentation
	"2002::/16",         // 6to4 can encode a forbidden IPv4 destination
	"2620:4f:8000::/48", // AS112 special-purpose service
	"3fff::/20",         // documentation (RFC 9637)
)

// prefixes parses the fixed reviewed CIDR table, failing fast on programmer errors.
func prefixes(values ...string) []netip.Prefix {
	result := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		result = append(result, netip.MustParsePrefix(value))
	}
	return result
}

// publicAddress accepts supported public origins after normalization and conservative exclusions.
func publicAddress(ip netip.Addr) bool {
	if !ip.IsValid() || ip.Zone() != "" {
		return false
	}
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() {
		return false
	}
	ranges := deniedIPv4
	if ip.Is6() {
		if !globalIPv6.Contains(ip) {
			return false
		}
		ranges = deniedIPv6
	}
	for _, block := range ranges {
		if block.Contains(ip) {
			return false
		}
	}
	return true
}
