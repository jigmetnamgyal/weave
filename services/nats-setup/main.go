// Command nats-setup generates local NATS credentials: an operator, a system
// account, the application account with its runner signing key, the server
// configuration trusting them, and a credential for every service identity
// (Unit M5.5a, ADR-014).
//
// For local development and tests only. The output is secret — seeds — and is
// written to a gitignored directory with owner-only permissions. Generated,
// never committed: a committed seed is a secret in the history.
//
// Idempotent: an existing setup is left alone unless -force is given, because
// regenerating invalidates every credential issued from the old one.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/jigmetnamgyal/weave/internal/adapters/natsauth"
)

func main() {
	out := flag.String("out", "infra/nats/generated", "directory to write the setup into")
	force := flag.Bool("force", false, "regenerate even if a setup exists")
	flag.Parse()

	// The complete set, not one file: a setup interrupted partway is
	// regenerated rather than trusted.
	if natsauth.Complete(*out) && !*force {
		fmt.Printf("NATS credentials already exist in %s\n", *out)
		return
	}
	if _, err := natsauth.Generate(*out); err != nil {
		fmt.Fprintf(os.Stderr, "nats-setup: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Generated NATS credentials in %s\n", *out)
}
