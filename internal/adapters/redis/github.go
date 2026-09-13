// Package redis holds the short-lived, expiring state the product needs but
// must not keep.
//
// Everything here is chosen for Redis precisely because Redis forgets. An
// install-state value is single-use and dead in fifteen minutes; an
// installation token is a live credential for customer source code that GitHub
// expires after an hour. Putting either in PostgreSQL would keep them long
// past their usefulness and turn a database backup into a set of working
// credentials.
package redis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// installStatePrefix namespaces install-state keys.
const installStatePrefix = "github:install-state:"

// InstallStateStore keeps install-state values for the length of one
// installation round trip.
type InstallStateStore struct {
	client *redis.Client
}

// NewInstallStateStore returns a store backed by client.
func NewInstallStateStore(client *redis.Client) *InstallStateStore {
	return &InstallStateStore{client: client}
}

// storedState is the serialised form. The random value is the key, so it is
// not repeated in the payload.
type storedState struct {
	WorkspaceID string `json:"workspace_id"`
	UserID      string `json:"user_id"`
}

// Put stores a state under a time to live.
func (s *InstallStateStore) Put(ctx context.Context, state domain.InstallState, ttl time.Duration) error {
	payload, err := json.Marshal(storedState{
		WorkspaceID: state.WorkspaceID.String(),
		UserID:      state.UserID.String(),
	})
	if err != nil {
		return fmt.Errorf("encode install state: %w", err)
	}
	if err := s.client.Set(ctx, installStatePrefix+state.Value, payload, ttl).Err(); err != nil {
		return fmt.Errorf("store install state: %w", err)
	}
	return nil
}

// Take returns a state and removes it in the same operation.
//
// GETDEL, not GET followed by DEL. Two requests replaying the same callback
// concurrently must not both succeed, and a read-then-delete leaves exactly
// that window open — which is the window in which one installation gets bound
// twice.
func (s *InstallStateStore) Take(ctx context.Context, value string) (domain.InstallState, error) {
	if value == "" {
		return domain.InstallState{}, application.ErrInstallStateInvalid
	}

	payload, err := s.client.GetDel(ctx, installStatePrefix+value).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return domain.InstallState{}, application.ErrInstallStateInvalid
		}
		return domain.InstallState{}, fmt.Errorf("take install state: %w", err)
	}

	var stored storedState
	if err := json.Unmarshal(payload, &stored); err != nil {
		return domain.InstallState{}, fmt.Errorf("decode install state: %w", err)
	}

	state := domain.InstallState{Value: value}
	if state.WorkspaceID, err = parseUUID(stored.WorkspaceID); err != nil {
		return domain.InstallState{}, err
	}
	if state.UserID, err = parseUUID(stored.UserID); err != nil {
		return domain.InstallState{}, err
	}
	return state, nil
}

// TokenCache stores GitHub installation tokens for their lifetime.
type TokenCache struct {
	client *redis.Client
}

// NewTokenCache returns a cache backed by client.
func NewTokenCache(client *redis.Client) *TokenCache {
	return &TokenCache{client: client}
}

// Get returns a cached token, or "" when absent.
//
// A miss and a failure are both reported as "" with no error, because the
// caller's response to each is the same: mint a fresh token. Redis being down
// should make GitHub calls slower, not impossible.
func (c *TokenCache) Get(ctx context.Context, key string) (string, error) {
	token, err := c.client.Get(ctx, key).Result()
	if err != nil {
		return "", nil //nolint:nilerr // a cache miss and a cache outage are the same to the caller
	}
	return token, nil
}

// Set stores a token under a time to live.
//
// The TTL is not optional and there is no path that writes without one: these
// are credentials that reach customer repositories, and one left in the cache
// after GitHub expired it is a credential nobody is tracking.
func (c *TokenCache) Set(ctx context.Context, key, token string, ttl time.Duration) error {
	if ttl <= 0 {
		return errors.New("redis: refusing to cache an installation token without an expiry")
	}
	return c.client.Set(ctx, key, token, ttl).Err()
}
