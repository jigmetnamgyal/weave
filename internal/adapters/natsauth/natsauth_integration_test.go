package natsauth_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/jigmetnamgyal/weave/internal/adapters/eventstream"
	"github.com/jigmetnamgyal/weave/internal/adapters/natsauth"
)

// authWorld is a real, authenticated NATS server and the generated setup that
// configures it. Every refusal below is the broker's, not our code's.
type authWorld struct {
	url    string
	dir    string
	issuer *natsauth.Issuer
	cfg    eventstream.Config
	admin  *nats.Conn
}

func newAuthWorld(t *testing.T) *authWorld {
	t.Helper()
	url, dir := os.Getenv("TEST_NATS_URL"), os.Getenv("TEST_NATS_AUTH_DIR")
	if url == "" || dir == "" {
		t.Skip("TEST_NATS_URL and TEST_NATS_AUTH_DIR are not set; run `make test-integration`")
	}
	id := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	cfg := eventstream.Config{
		Stream: "TEST_AUTH_" + id, SubjectPrefix: "weavetest" + id + ".session",
		Consumer: "test-ingestor", AckWait: 2 * time.Second, ExhaustAfter: 3,
	}
	issuer, err := natsauth.LoadIssuer(filepath.Join(dir, natsauth.RunnerSigningFile),
		filepath.Join(dir, natsauth.AccountPublicFile), cfg.SubjectPrefix)
	if err != nil {
		t.Fatalf("load issuer: %v", err)
	}
	admin, err := nats.Connect(url, nats.UserCredentials(filepath.Join(dir, "tests.creds")))
	if err != nil {
		t.Fatalf("connect as the tests identity: %v", err)
	}
	t.Cleanup(admin.Close)
	js, _ := jetstream.New(admin)
	if err := eventstream.EnsureStream(context.Background(), js, cfg); err != nil {
		t.Fatalf("test stream: %v", err)
	}
	t.Cleanup(func() { _ = js.DeleteStream(context.Background(), cfg.Stream) })
	return &authWorld{url: url, dir: dir, issuer: issuer, cfg: cfg, admin: admin}
}

// connect opens a connection with a credential, collecting the broker's
// asynchronous errors — which is where a permissions violation arrives.
func (w *authWorld) connect(t *testing.T, creds string, opts ...nats.Option) (*nats.Conn, <-chan error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "user.creds")
	if err := os.WriteFile(path, []byte(creds), 0o600); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 16)
	opts = append(opts, nats.UserCredentials(path),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			select {
			case errs <- err:
			default:
			}
		}))
	conn, err := nats.Connect(w.url, opts...)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	return conn, errs
}

// refused reports whether the broker sent a permissions violation.
func refused(t *testing.T, conn *nats.Conn, errs <-chan error) bool {
	t.Helper()
	_ = conn.FlushTimeout(3 * time.Second)
	select {
	case err := <-errs:
		return errors.Is(err, nats.ErrPermissionViolation) ||
			strings.Contains(strings.ToLower(err.Error()), "permissions violation")
	case <-time.After(3 * time.Second):
		return false
	}
}

func (w *authWorld) runner(t *testing.T) (uuid.UUID, uuid.UUID, string) {
	t.Helper()
	runner, session := uuid.New(), uuid.New()
	creds, err := w.issuer.IssueRunner(runner, session)
	if err != nil {
		t.Fatal(err)
	}
	return runner, session, creds.Creds
}

func TestAnUnauthenticatedConnectionIsRefusedIntegration(t *testing.T) {
	w := newAuthWorld(t)
	if conn, err := nats.Connect(w.url); err == nil {
		conn.Close()
		t.Fatal("an unauthenticated connection was accepted; authentication is not on")
	}
}

// TestARunnerPublishesOnlyItsOwnSessionIntegration is the broker-side half of
// M5.3's gate, which M5.4a could not close: a runner's credential publishes
// its own session's events, and the broker — not the ingestor — refuses
// another session's.
func TestARunnerPublishesOnlyItsOwnSessionIntegration(t *testing.T) {
	w := newAuthWorld(t)
	runner, session, creds := w.runner(t)
	conn, errs := w.connect(t, creds, nats.CustomInboxPrefix(natsauth.RunnerInboxPrefix(runner)))

	js, _ := jetstream.New(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	own := w.cfg.SubjectPrefix + "." + session.String() + ".events"
	if _, err := js.Publish(ctx, own, []byte("{}")); err != nil {
		t.Fatalf("publishing its own session's subject: %v", err)
	}

	other := w.cfg.SubjectPrefix + "." + uuid.NewString() + ".events"
	if err := conn.Publish(other, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if !refused(t, conn, errs) {
		t.Error("the broker let a runner publish another session's subject")
	}
}

func TestARunnerCannotReadTheStreamOrAnotherInboxIntegration(t *testing.T) {
	w := newAuthWorld(t)
	for name, subject := range map[string]string{
		"the event stream":        w.cfg.SubjectPrefix + ".*.events",
		"another runner's inbox":  natsauth.RunnerInboxPrefix(uuid.New()) + ".>",
		"the default inbox space": "_INBOX.>",
	} {
		t.Run(name, func(t *testing.T) {
			runner, _, creds := w.runner(t)
			conn, errs := w.connect(t, creds, nats.CustomInboxPrefix(natsauth.RunnerInboxPrefix(runner)))
			if _, err := conn.SubscribeSync(subject); err != nil {
				t.Fatal(err)
			}
			if !refused(t, conn, errs) {
				t.Errorf("a runner was allowed to subscribe to %s", subject)
			}
		})
	}
}

func TestAnExpiredRunnerCredentialIsRefusedIntegration(t *testing.T) {
	w := newAuthWorld(t)
	seed, _ := os.ReadFile(filepath.Join(w.dir, natsauth.RunnerSigningFile))
	account, _ := os.ReadFile(filepath.Join(w.dir, natsauth.AccountPublicFile))
	past := time.Now().Add(-natsauth.RunnerCredentialLifetime - time.Hour)
	expired, err := natsauth.NewIssuer([]byte(strings.TrimSpace(string(seed))), strings.TrimSpace(string(account)),
		w.cfg.SubjectPrefix, func() time.Time { return past })
	if err != nil {
		t.Fatal(err)
	}
	creds, err := expired.IssueRunner(uuid.New(), uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "expired.creds")
	_ = os.WriteFile(path, []byte(creds.Creds), 0o600)
	if conn, err := nats.Connect(w.url, nats.UserCredentials(path)); err == nil {
		conn.Close()
		t.Error("an expired credential was accepted")
	}
}

// TestTheIngestorAllowListIsSufficientAndNoWiderIntegration: minted for a
// stream of the test's own, the ingestor's allow-list creates the stream and
// its consumer, fetches and acknowledges — everything the ingestor does — and
// cannot publish a session event.
func TestTheIngestorAllowListIsSufficientAndNoWiderIntegration(t *testing.T) {
	w := newAuthWorld(t)
	creds, err := w.issuer.ServiceCredentials(natsauth.IdentityIngestor, natsauth.IngestorPermissions(w.cfg.Stream))
	if err != nil {
		t.Fatal(err)
	}
	conn, errs := w.connect(t, creds)
	js, _ := jetstream.New(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := eventstream.EnsureStream(ctx, js, w.cfg); err != nil {
		t.Fatalf("the ingestor could not verify its stream: %v", err)
	}
	consumer, err := js.CreateOrUpdateConsumer(ctx, w.cfg.Stream, jetstream.ConsumerConfig{
		Durable: w.cfg.Consumer, FilterSubject: w.cfg.SubjectPrefix + ".*.events",
		AckPolicy: jetstream.AckExplicitPolicy,
	})
	if err != nil {
		t.Fatalf("the ingestor could not create its consumer: %v", err)
	}
	adminJS, _ := jetstream.New(w.admin)
	if _, err := adminJS.Publish(ctx, w.cfg.SubjectPrefix+"."+uuid.NewString()+".events", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	batch, err := consumer.Fetch(1, jetstream.FetchMaxWait(5*time.Second))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	got := 0
	for msg := range batch.Messages() {
		if err := msg.DoubleAck(ctx); err != nil {
			t.Fatalf("the ingestor could not acknowledge: %v", err)
		}
		got++
	}
	if got != 1 {
		t.Fatalf("fetched %d messages, want 1", got)
	}

	if err := conn.Publish(w.cfg.SubjectPrefix+"."+uuid.NewString()+".events", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if !refused(t, conn, errs) {
		t.Error("the ingestor could publish a session event; it reads events and never writes them")
	}
}

// TestTheAPIAndRunnerManagerAreRefusedOutsideTheirAllowListsIntegration.
func TestTheAPIAndRunnerManagerAreRefusedOutsideTheirAllowListsIntegration(t *testing.T) {
	w := newAuthWorld(t)

	apiCreds, _ := os.ReadFile(filepath.Join(w.dir, "api.creds"))
	api, apiErrs := w.connect(t, string(apiCreds))
	if err := api.FlushTimeout(3 * time.Second); err != nil {
		t.Fatalf("the API's readiness round trip failed: %v", err)
	}
	apiJS, _ := jetstream.New(api)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := apiJS.AccountInfo(ctx); err == nil {
		t.Error("the API could call the JetStream API")
	}
	if _, err := api.SubscribeSync(w.cfg.SubjectPrefix + ".*.events"); err != nil {
		t.Fatal(err)
	}
	if !refused(t, api, apiErrs) {
		t.Error("the API could subscribe to session events")
	}

	managerCreds, err := w.issuer.ServiceCredentials(natsauth.IdentityRunnerManager,
		natsauth.RunnerManagerPermissions(w.cfg.Stream))
	if err != nil {
		t.Fatal(err)
	}
	manager, managerErrs := w.connect(t, managerCreds)
	managerJS, _ := jetstream.New(manager)
	infoCtx, infoCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer infoCancel()
	if _, err := managerJS.Stream(infoCtx, w.cfg.Stream); err != nil {
		t.Errorf("the runner manager could not read its stream's information: %v", err)
	}
	if err := manager.Publish(w.cfg.SubjectPrefix+"."+uuid.NewString()+".events", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if !refused(t, manager, managerErrs) {
		t.Error("the runner manager could publish a session event")
	}
}
