package natsauth_test

import (
	"errors"
	"strings"
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
