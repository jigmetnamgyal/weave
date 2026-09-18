package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// systemActorPattern matches any way of claiming to be the system.
//
// Both the constructor and the raw literal, because the constructor is only a
// convention and a struct literal setting the field would bypass it.
var systemActorPattern = regexp.MustCompile(`SystemActor\(\)|System:\s*true`)

// TestNoHandlerActsAsTheSystem is the guard a comment in
// internal/application/workspaces.go promises.
//
// It did not exist when that comment was written, which a reviewer caught: the
// comment cited a regression gate by name and there was none. A comment naming
// a protection that does not exist is worse than no comment, because the next
// reader stops looking.
//
// The boundary it defends: `Actor.System` skips the membership and permission
// re-check entirely. That is correct for a workflow — the product acting on
// its own — and would be a privilege escalation from a request handler, where
// it would let any caller write as though authorized. Nothing reachable from
// HTTP may construct one.
//
// A textual check, which is worth knowing the limits of: it sees the API's own
// source and would miss a system actor built in a package the API calls. It is
// the same shape as TestEveryRouteIsDescribedByTheContract, and the same
// caveat applies — it catches the mistake someone actually makes, which is
// reaching for the shortcut while editing a handler.
func TestNoHandlerActsAsTheSystem(t *testing.T) {
	root := filepath.Join("..", "..", "services", "api")

	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		// This file names the pattern in order to search for it.
		if strings.HasSuffix(path, "system_actor_test.go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, match := range systemActorPattern.FindAllString(string(source), -1) {
			offenders = append(offenders, path+": "+match)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan api source: %v", err)
	}

	for _, offender := range offenders {
		t.Errorf("%s — a request handler must never act as the system: it skips the "+
			"membership and permission re-check, which is authorization for a workflow "+
			"and a privilege escalation for a caller", offender)
	}
}
