package application

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/domain"
)

// fakeStore is an in-memory UserStore that mimics the unique constraint on
// external_id, so provisioning logic can be exercised without a database.
type fakeStore struct {
	mu       sync.Mutex
	byExtID  map[string]domain.User
	findErr  error
	writeErr error
	upserts  int
	finds    int
}

func newFakeStore() *fakeStore {
	return &fakeStore{byExtID: make(map[string]domain.User)}
}

func (f *fakeStore) FindByExternalID(_ context.Context, externalID string) (domain.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.finds++
	if f.findErr != nil {
		return domain.User{}, f.findErr
	}
	user, ok := f.byExtID[externalID]
	if !ok {
		return domain.User{}, ErrUserNotFound
	}
	return user, nil
}

func (f *fakeStore) UpsertByExternalID(_ context.Context, user domain.User) (domain.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.upserts++
	if f.writeErr != nil {
		return domain.User{}, f.writeErr
	}
	// The unique constraint: an existing row wins, the candidate ID is discarded.
	if existing, ok := f.byExtID[user.ExternalID]; ok {
		return existing, nil
	}
	f.byExtID[user.ExternalID] = user
	return user, nil
}

func testIdentity() Identity {
	return Identity{
		Subject:     "user_2abcXYZ",
		Email:       "dev@example.com",
		DisplayName: "Ada Lovelace",
	}
}

func TestFromIdentityCreatesUserOnFirstSignIn(t *testing.T) {
	store := newFakeStore()
	provisioner := NewUserProvisioner(store)

	user, err := provisioner.FromIdentity(context.Background(), testIdentity())
	if err != nil {
		t.Fatalf("FromIdentity() returned error: %v", err)
	}

	if user.ID == uuid.Nil {
		t.Error("created user has no id")
	}
	if user.ExternalID != testIdentity().Subject {
		t.Errorf("ExternalID = %q, want %q", user.ExternalID, testIdentity().Subject)
	}
	if user.Email != testIdentity().Email {
		t.Errorf("Email = %q, want %q", user.Email, testIdentity().Email)
	}
	if len(store.byExtID) != 1 {
		t.Errorf("store holds %d users, want 1", len(store.byExtID))
	}
}

// TestFromIdentityReusesExistingUser proves a returning caller resolves to the
// same row, and costs no write.
func TestFromIdentityReusesExistingUser(t *testing.T) {
	store := newFakeStore()
	provisioner := NewUserProvisioner(store)
	ctx := context.Background()

	first, err := provisioner.FromIdentity(ctx, testIdentity())
	if err != nil {
		t.Fatalf("first FromIdentity() returned error: %v", err)
	}
	upsertsAfterFirst := store.upserts

	second, err := provisioner.FromIdentity(ctx, testIdentity())
	if err != nil {
		t.Fatalf("second FromIdentity() returned error: %v", err)
	}

	if first.ID != second.ID {
		t.Errorf("second sign-in produced a different user: %s then %s", first.ID, second.ID)
	}
	if len(store.byExtID) != 1 {
		t.Errorf("store holds %d users, want 1", len(store.byExtID))
	}
	if store.upserts != upsertsAfterFirst {
		t.Errorf("returning sign-in performed %d extra writes, want 0", store.upserts-upsertsAfterFirst)
	}
}

// TestFromIdentityIsSafeUnderConcurrentFirstRequests proves the read-then-
// upsert sequence still yields one user when many requests race.
func TestFromIdentityIsSafeUnderConcurrentFirstRequests(t *testing.T) {
	store := newFakeStore()
	provisioner := NewUserProvisioner(store)

	const concurrency = 16

	var wg sync.WaitGroup
	ids := make([]uuid.UUID, concurrency)
	errs := make([]error, concurrency)
	start := make(chan struct{})

	for i := range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			user, err := provisioner.FromIdentity(context.Background(), testIdentity())
			ids[i], errs[i] = user.ID, err
		}()
	}

	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d returned error: %v", i, err)
		}
	}
	for i, id := range ids {
		if id != ids[0] {
			t.Errorf("goroutine %d resolved to user %s, want %s", i, id, ids[0])
		}
	}
	if len(store.byExtID) != 1 {
		t.Errorf("store holds %d users, want 1", len(store.byExtID))
	}
}

func TestFromIdentityRejectsIdentityWithoutEmail(t *testing.T) {
	store := newFakeStore()
	provisioner := NewUserProvisioner(store)

	identity := testIdentity()
	identity.Email = ""

	if _, err := provisioner.FromIdentity(context.Background(), identity); !errors.Is(err, domain.ErrInvalidUser) {
		t.Errorf("error = %v, want domain.ErrInvalidUser", err)
	}
	if len(store.byExtID) != 0 {
		t.Error("an invalid user was written to the store")
	}
}

// TestFromIdentityPropagatesLookupFailure proves a storage outage is not
// mistaken for "this user does not exist yet", which would attempt a write on
// every request during an incident.
func TestFromIdentityPropagatesLookupFailure(t *testing.T) {
	store := newFakeStore()
	store.findErr = errors.New("connection refused")
	provisioner := NewUserProvisioner(store)

	if _, err := provisioner.FromIdentity(context.Background(), testIdentity()); err == nil {
		t.Fatal("FromIdentity() ignored a lookup failure")
	}
	if store.upserts != 0 {
		t.Errorf("attempted %d writes after a failed lookup, want 0", store.upserts)
	}
}
