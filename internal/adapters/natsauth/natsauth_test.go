package natsauth_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nkeys"

	"github.com/jigmetnamgyal/weave/internal/adapters/natsauth"
)

func issuer(t *testing.T, now func() time.Time) (*natsauth.Issuer, string) {
	t.Helper()
	account, _ := nkeys.CreateAccount()
	accountPub, _ := account.PublicKey()
	signing, _ := nkeys.CreateAccount()
	seed, _ := signing.Seed()
	i, err := natsauth.NewIssuer(seed, accountPub, "weave.session", now)
	if err != nil {
		t.Fatal(err)
	}
	return i, accountPub
}

// TestARunnerCredentialNamesOneSubjectAndNothingElse reads back the claims a
// runner credential carries — the whole of the scope the broker enforces.
func TestARunnerCredentialNamesOneSubjectAndNothingElse(t *testing.T) {
	now := time.Now()
	i, account := issuer(t, func() time.Time { return now })
	runner, session := uuid.New(), uuid.New()

	creds, err := i.IssueRunner(runner, session)
	if err != nil {
		t.Fatal(err)
	}
	token, err := jwt.ParseDecoratedJWT([]byte(creds.Creds))
	if err != nil {
		t.Fatal(err)
	}
	claims, err := jwt.DecodeUserClaims(token)
	if err != nil {
		t.Fatal(err)
	}

	wantSubject := "weave.session." + session.String() + ".events"
	if got := claims.Pub.Allow; len(got) != 1 || got[0] != wantSubject {
		t.Errorf("publish allow = %v, want only %s", got, wantSubject)
	}
	wantInbox := natsauth.RunnerInboxPrefix(runner) + ".>"
	if got := claims.Sub.Allow; len(got) != 1 || got[0] != wantInbox {
		t.Errorf("subscribe allow = %v, want only its own inbox %s", got, wantInbox)
	}
	if claims.IssuerAccount != account {
		t.Errorf("issuer account = %s, want %s", claims.IssuerAccount, account)
	}
	if want := now.Add(natsauth.RunnerCredentialLifetime).Unix(); claims.Expires != want {
		t.Errorf("expires = %d, want %d", claims.Expires, want)
	}
	if claims.Name != "runner-"+runner.String() {
		t.Errorf("name = %q; it must name the runner and nothing about a customer", claims.Name)
	}
	if creds.Subject != wantSubject || !strings.HasPrefix(wantInbox, creds.InboxPrefix) {
		t.Errorf("returned %+v, want the same subject and inbox the claims carry", creds)
	}
}

func TestTheIssuerRefusesAKeyThatIsNotAnAccountSigningKey(t *testing.T) {
	account, _ := nkeys.CreateAccount()
	accountPub, _ := account.PublicKey()
	for name, make := range map[string]func() (nkeys.KeyPair, error){
		"an operator key": nkeys.CreateOperator,
		"a user key":      nkeys.CreateUser,
	} {
		kp, _ := make()
		seed, _ := kp.Seed()
		if _, err := natsauth.NewIssuer(seed, accountPub, "weave.session", time.Now); err == nil {
			t.Errorf("%s was accepted as the signing key", name)
		}
	}
}

// TestCredentialPathsDefaultOnlyInDevelopment: a deployment that forgets a
// credential must fail to start, not connect unauthenticated or with a
// developer's key.
func TestCredentialPathsDefaultOnlyInDevelopment(t *testing.T) {
	for _, env := range []string{"development", "test"} {
		if got, err := natsauth.ResolvePath(env, "", "api.creds"); err != nil || got == "" {
			t.Errorf("%s: %q %v, want the generated default", env, got, err)
		}
	}
	for _, env := range []string{"staging", "production", "prod", ""} {
		if _, err := natsauth.ResolvePath(env, "", "api.creds"); !errors.Is(err, natsauth.ErrCredentialsRequired) {
			t.Errorf("APP_ENV=%q: %v, want ErrCredentialsRequired", env, err)
		}
	}
	if got, _ := natsauth.ResolvePath("production", "/secrets/api.creds", "api.creds"); got != "/secrets/api.creds" {
		t.Errorf("a configured path was not used: %q", got)
	}
}

// TestTheAPIHoldsNoSubjectPermissions: its connection is for a readiness PING,
// which needs none.
func TestTheAPIHoldsNoSubjectPermissions(t *testing.T) {
	p := natsauth.ServicePermissions[natsauth.IdentityAPI]
	if len(p.Publish) != 0 || len(p.Subscribe) != 0 {
		t.Errorf("API permissions = %+v, want none", p)
	}
	for _, subject := range natsauth.ServicePermissions[natsauth.IdentityIngestor].Publish {
		if strings.HasPrefix(subject, "weave.") {
			t.Errorf("the ingestor may publish %s; it reads events and never writes them", subject)
		}
	}
}

// TestAnInterruptedSetupIsRegeneratedNotTrusted is a review finding on PR #20.
// The first version wrote server.conf first and treated its existence as a
// complete setup, so a run stopped before the credentials left a setup no
// re-run would repair.
func TestAnInterruptedSetupIsRegeneratedNotTrusted(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "generated")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// What an interrupted run used to leave: the server config and nothing else.
	if err := os.WriteFile(filepath.Join(dir, natsauth.ServerConfigFile), []byte("operator: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if natsauth.Complete(dir) {
		t.Fatal("a setup holding only server.conf was taken as complete")
	}

	if _, err := natsauth.Generate(dir); err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	if !natsauth.Complete(dir) {
		t.Error("a fresh setup is not complete")
	}
	entries, _ := os.ReadDir(root)
	for _, entry := range entries {
		if entry.Name() != "generated" {
			t.Errorf("generation left %q behind; it must be all or nothing", entry.Name())
		}
	}
	info, err := os.Stat(filepath.Join(dir, natsauth.RunnerSigningFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the signing key is %v (%v); it must be owner-only", info.Mode().Perm(), err)
	}
}

// TestConcurrentSetupsGenerateOnce is a review finding on PR #20: `make up` and
// `make dev` started together could each see no setup, each generate, and the
// second replace the first's keys after NATS had loaded them. Under the lock,
// exactly one generates and every caller ends with the same key set.
func TestConcurrentSetupsGenerateOnce(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "generated")
	const callers = 8
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		generated int
		failures  []error
		start     = make(chan struct{})
	)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			did, err := natsauth.EnsureSetup(dir, false)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, err)
			} else if did {
				generated++
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(failures) > 0 {
		t.Fatalf("%d setups failed, first: %v", len(failures), failures[0])
	}
	if generated != 1 {
		t.Errorf("%d concurrent setups generated keys, want exactly 1", generated)
	}
	account, err := os.ReadFile(filepath.Join(dir, natsauth.AccountPublicFile))
	if err != nil || len(strings.TrimSpace(string(account))) == 0 {
		t.Fatalf("no account key after setup: %v", err)
	}
	// A later call finds the complete setup and leaves it alone.
	if did, err := natsauth.EnsureSetup(dir, false); err != nil || did {
		t.Errorf("a setup over a complete one = (%v, %v), want left alone", did, err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, natsauth.AccountPublicFile))
	if string(after) != string(account) {
		t.Error("the account key changed after the setup was complete")
	}
}
