package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jigmetnamgyal/weave/internal/adapters/postgres"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// newPool connects to the database named by TEST_DATABASE_URL.
//
// These tests exercise a guarantee that only a real database can demonstrate:
// that the unique constraint, not application code, is what stops concurrent
// first requests creating two users. A fake store can assert the intent but
// never the mechanism. They skip when no database is configured so `go test`
// stays runnable without Docker; `make test-integration` supplies the URL.
func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set; run `make test-integration`")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping test database: %v (is `make up` running and `make migrate-up` applied?)", err)
	}
	t.Cleanup(pool.Close)

	return pool
}

// uniqueSubject keeps parallel tests from colliding on external_id.
func uniqueSubject(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("user_test_%s", uuid.NewString())
}

// cleanup removes rows this test created.
func cleanup(t *testing.T, pool *pgxpool.Pool, externalID string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.Exec(ctx, "DELETE FROM users WHERE external_id = $1", externalID); err != nil {
			t.Errorf("cleanup user %s: %v", externalID, err)
		}
	})
}

func newUser(t *testing.T, externalID string) domain.User {
	t.Helper()
	id, err := domain.NewUserID()
	if err != nil {
		t.Fatalf("generate user id: %v", err)
	}
	return domain.User{
		ID:          id,
		ExternalID:  externalID,
		Email:       externalID + "@example.com",
		DisplayName: "Ada Lovelace",
	}
}

func TestFindByExternalIDReportsMissingUser(t *testing.T) {
	store := postgres.NewUserStore(newPool(t))

	_, err := store.FindByExternalID(context.Background(), uniqueSubject(t))
	if !errors.Is(err, application.ErrUserNotFound) {
		t.Errorf("error = %v, want application.ErrUserNotFound", err)
	}
}

func TestUpsertCreatesThenReturnsSameRow(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewUserStore(pool)
	ctx := context.Background()

	subject := uniqueSubject(t)
	cleanup(t, pool, subject)

	created, err := store.UpsertByExternalID(ctx, newUser(t, subject))
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	found, err := store.FindByExternalID(ctx, subject)
	if err != nil {
		t.Fatalf("find after upsert: %v", err)
	}
	if found.ID != created.ID {
		t.Errorf("found user %s, want %s", found.ID, created.ID)
	}
	if found.DisplayName != "Ada Lovelace" {
		t.Errorf("DisplayName = %q, want %q", found.DisplayName, "Ada Lovelace")
	}

	// A second upsert carries a different candidate ID. The existing row must
	// win: the identity provider's subject already maps to a user, and the ID
	// is what everything else will reference.
	second := newUser(t, subject)
	second.DisplayName = "Ada L."
	updated, err := store.UpsertByExternalID(ctx, second)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if updated.ID != created.ID {
		t.Errorf("second upsert changed the user id from %s to %s", created.ID, updated.ID)
	}
	if updated.DisplayName != "Ada L." {
		t.Errorf("DisplayName = %q, want the refreshed %q", updated.DisplayName, "Ada L.")
	}
}

// TestConcurrentFirstRequestsCreateOneUser is the reason these tests need a
// real database: it proves the unique constraint resolves the race.
func TestConcurrentFirstRequestsCreateOneUser(t *testing.T) {
	pool := newPool(t)
	provisioner := application.NewUserProvisioner(postgres.NewUserStore(pool))

	subject := uniqueSubject(t)
	cleanup(t, pool, subject)

	identity := application.Identity{
		Subject:     subject,
		Email:       subject + "@example.com",
		DisplayName: "Ada Lovelace",
	}

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
			user, err := provisioner.FromIdentity(context.Background(), identity)
			ids[i], errs[i] = user.ID, err
		}()
	}

	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	for i, id := range ids {
		if id != ids[0] {
			t.Errorf("goroutine %d resolved to %s, want %s", i, id, ids[0])
		}
	}

	var count int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM users WHERE external_id = $1", subject).Scan(&count); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if count != 1 {
		t.Errorf("%d rows created for one subject, want 1", count)
	}
}

// TestEmailIsCaseInsensitive covers the citext column type.
func TestEmailIsCaseInsensitive(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewUserStore(pool)
	ctx := context.Background()

	subject := uniqueSubject(t)
	cleanup(t, pool, subject)

	user := newUser(t, subject)
	user.Email = "Ada.Lovelace@Example.COM"
	if _, err := store.UpsertByExternalID(ctx, user); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM users WHERE email = $1", "ada.lovelace@example.com").Scan(&count); err != nil {
		t.Fatalf("query by lowercased email: %v", err)
	}
	if count != 1 {
		t.Errorf("case-insensitive email lookup found %d rows, want 1", count)
	}
}

// TestOptionalProfileFieldsBecomeNull proves an absent display name is stored
// as NULL rather than as an empty string that reads like a real value.
func TestOptionalProfileFieldsBecomeNull(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewUserStore(pool)
	ctx := context.Background()

	subject := uniqueSubject(t)
	cleanup(t, pool, subject)

	user := newUser(t, subject)
	user.DisplayName = ""
	user.AvatarURL = ""
	if _, err := store.UpsertByExternalID(ctx, user); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	var displayNameIsNull, avatarIsNull bool
	if err := pool.QueryRow(ctx,
		"SELECT display_name IS NULL, avatar_url IS NULL FROM users WHERE external_id = $1", subject,
	).Scan(&displayNameIsNull, &avatarIsNull); err != nil {
		t.Fatalf("query null-ness: %v", err)
	}
	if !displayNameIsNull {
		t.Error("empty display name was stored as a value, want NULL")
	}
	if !avatarIsNull {
		t.Error("empty avatar url was stored as a value, want NULL")
	}

	// It must still read back as an empty string, not a surprise pointer.
	found, err := store.FindByExternalID(ctx, subject)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if found.DisplayName != "" || found.AvatarURL != "" {
		t.Errorf("NULL fields read back as %q / %q, want empty strings", found.DisplayName, found.AvatarURL)
	}
}
