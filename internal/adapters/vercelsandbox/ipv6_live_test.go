//go:build vercel_live

package vercelsandbox

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestLiveIPv6ConnectivitySnapshot characterizes the current sandbox image and
// network. It is not a durable provider guarantee: M5.4d still needs a supported
// mechanism that prevents forbidden IPv6 destinations when arbitrary hosts are
// allowed. An IPv6-capable public host supplies the IPv4 control.
func TestLiveIPv6ConnectivitySnapshot(t *testing.T) {
	b := liveBackend(t, "live"+uuid.NewString()[:6], DefaultLease)
	_, session := sandbox(t, b, plainRules("www.cloudflare.com"))
	ctx := context.Background()
	for _, c := range []struct {
		name, script string
	}{
		{"IPv6 configuration", `python3 -c 'import json,pathlib; paths=["/proc/sys/net/ipv6/conf/all/disable_ipv6","/proc/sys/net/ipv6/conf/default/disable_ipv6","/proc/net/if_inet6","/proc/net/ipv6_route"]; print(json.dumps({p:pathlib.Path(p).read_text().strip() if pathlib.Path(p).exists() else "absent" for p in paths}))'`},
		{"AAAA resolution", `getent ahostsv6 www.cloudflare.com`},
		{"IPv4 control", `set -o pipefail; curl -4 -sS --max-time 15 -D - -o /dev/null https://www.cloudflare.com 2>/dev/null | grep -i '^server:'`},
		{"IPv6 request", `curl -6 -sS --max-time 15 -w '%{http_code}\n' -o /dev/null https://www.cloudflare.com 2>&1`},
	} {
		exit, out, err := b.LiveRun(ctx, session, c.script)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: exit=%d output=%q", c.name, exit, strings.TrimSpace(out))
		if c.name == "IPv6 configuration" && exit != 0 {
			t.Fatal("configuration probe failed")
		}
		if c.name == "IPv4 control" && (exit != 0 || !strings.Contains(strings.ToLower(out), "server: cloudflare")) {
			t.Fatal("public-host IPv4 control did not receive a Cloudflare response")
		}
	}
}
