package postgres_test

import (
	"errors"
	"github.com/jigmetnamgyal/weave/internal/application"
	"testing"

	"github.com/jigmetnamgyal/weave/internal/adapters/postgres"
)

// TestProviderCredentialStoreRequiresPool rejects a missing database dependency
// at construction, before an application service can reach a storage operation.
func TestProviderCredentialStoreRequiresPool(t *testing.T) {
	if store, err := postgres.NewProviderCredentialStore(nil); store != nil || !errors.Is(err, application.ErrCredentialConfiguration) {
		t.Fatal("missing pool accepted during construction")
	}
}
