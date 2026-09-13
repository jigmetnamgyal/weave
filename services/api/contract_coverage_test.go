package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestEveryRouteIsDescribedByTheContract compares the routes the API registers
// against the paths the OpenAPI contract describes, in both directions.
//
// It exists because nothing else stops the two diverging. During M3.1 seven
// endpoints were built, shipped and used while the contract knew nothing about
// them — and since the web application's types are generated from that
// contract, an undescribed endpoint is one the browser has no checked types
// for at all.
//
// **What this does not check.** Only that a path exists on both sides. It says
// nothing about whether the request and response *shapes* agree, which is the
// expensive half and deliberately out of scope: verifying that needs the
// handlers exercised against the schemas. A passing run here means the two
// lists match, and no more than that. Read it as nothing more.
func TestEveryRouteIsDescribedByTheContract(t *testing.T) {
	registered := routesFromSource(t)
	described := pathsFromContract(t)

	if len(registered) == 0 {
		t.Fatal("found no registered routes; the source scan is broken, not the contract")
	}

	for _, route := range registered {
		if !described[route] {
			t.Errorf("route %s is served but the contract does not describe it — "+
				"the web application has no generated types for it", route)
		}
	}
	for route := range described {
		if !contains(registered, route) {
			t.Errorf("the contract describes %s but no route serves it — "+
				"generated types exist for an endpoint that does not answer", route)
		}
	}
}

// routePattern matches the route literals the handlers register, as
// `mux.Handle("POST /v1/…"` or through the scoped helper.
var routePattern = regexp.MustCompile(`"(GET|POST|PATCH|PUT|DELETE) (/v1/[^"]*)"`)

// routesFromSource scans the API's Go source for registered route literals.
//
// Reading the source rather than the mux because http.ServeMux does not expose
// its patterns. That makes this a textual check, which is worth knowing: it
// sees a route the moment it is written, and would miss one registered through
// a computed string. Every route in this service is a literal, and a computed
// one should be treated as a reason to revisit this test rather than a clever
// thing to do.
func routesFromSource(t *testing.T) []string {
	t.Helper()

	seen := map[string]bool{}
	root := filepath.Join("..", "..", "services", "api")

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, match := range routePattern.FindAllStringSubmatch(string(source), -1) {
			seen[normalisePath(match[2])] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan api source: %v", err)
	}

	routes := make([]string, 0, len(seen))
	for route := range seen {
		routes = append(routes, route)
	}
	sort.Strings(routes)
	return routes
}

// pathsFromContract reads the paths the OpenAPI document describes.
func pathsFromContract(t *testing.T) map[string]bool {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("..", "..", "contracts", "openapi", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read contract: %v", err)
	}

	var document struct {
		Paths map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatalf("parse contract: %v", err)
	}

	described := make(map[string]bool, len(document.Paths))
	for path := range document.Paths {
		described[normalisePath(path)] = true
	}
	return described
}

// parameterSegment matches a path parameter in either source.
var parameterSegment = regexp.MustCompile(`\{[^}]*\}`)

// normalisePath reduces a path to its shape, so the two sources can be
// compared despite naming the same parameter differently — the Go routes use
// `{workspaceID}` and the contract `{workspaceId}`. The names are a real
// inconsistency, but not one this test should fail on: it is checking
// coverage, and treating a casing difference as a missing endpoint would bury
// the finding it exists to surface.
func normalisePath(path string) string {
	return parameterSegment.ReplaceAllString(strings.TrimSuffix(path, "/"), "{}")
}

func contains(haystack []string, needle string) bool {
	for _, candidate := range haystack {
		if candidate == needle {
			return true
		}
	}
	return false
}
