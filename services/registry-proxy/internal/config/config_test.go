package config

import (
	"strings"
	"testing"
)

func TestTheRegistryProxyNeedsItsPublicURLAndItsProject(t *testing.T) {
	set := func(env map[string]string) {
		for _, k := range []string{"APP_ENV", "APP_DATABASE_URL", "REGISTRY_PROXY_PUBLIC_URL",
			"REGISTRY_PROXY_VERCEL_TEAM_ID", "REGISTRY_PROXY_VERCEL_PROJECT_ID"} {
			t.Setenv(k, env[k])
		}
	}
	set(map[string]string{})
	_, err := Load()
	for _, want := range []string{"APP_ENV", "APP_DATABASE_URL", "REGISTRY_PROXY_PUBLIC_URL", "REGISTRY_PROXY_VERCEL_PROJECT_ID"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("missing %s not reported: %v", want, err)
		}
	}
	good := map[string]string{"APP_ENV": "development", "APP_DATABASE_URL": "postgres://x",
		"REGISTRY_PROXY_PUBLIC_URL": "https://quick-name.trycloudflare.com/", "REGISTRY_PROXY_VERCEL_TEAM_ID": "team_x",
		"REGISTRY_PROXY_VERCEL_PROJECT_ID": "prj_x"}
	set(good)
	cfg, err := Load()
	if err != nil || cfg.PublicURL != "https://quick-name.trycloudflare.com" {
		t.Errorf("= %+v, %v; want the public URL without its trailing slash", cfg, err)
	}
	good["REGISTRY_PROXY_PUBLIC_URL"] = "http://quick-name.trycloudflare.com"
	set(good)
	if _, err := Load(); err == nil {
		t.Error("a plain-http public URL was accepted")
	}
}
