// Package natsauth issues the NATS credentials Weave's processes and runners
// connect with (Unit M5.5a, ADR-014).
//
// Decentralized JWT authentication: one operator, a system account, and one
// application account every Weave identity lives in. The application account
// has a separate signing key, held only by the runner manager, which it uses
// to mint each runner a user whose permissions name one session's subject —
// without touching the server, which verifies the signature.
//
// Every service identity's permissions are an allow-list written here, in one
// place, so it can be read, reviewed and tested as a whole.
package natsauth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nkeys"

	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// StreamName is the production event stream, named here because the service
// allow-lists are written against it.
const StreamName = "SESSION_EVENTS"

// RunnerCredentialLifetime bounds how long a runner's credential is valid.
//
// Twelve hours: longer than any session this system is meant to run, so a
// live runner's credential never expires under it, and short enough that one
// which outlived its runner stops working the same day. Teardown destroys the
// sandbox holding it; this is the second line, not the first. See ADR-014 on
// why there is no active revocation.
const RunnerCredentialLifetime = 12 * time.Hour

// Identity is a service that connects to NATS.
type Identity string

// The service identities. Each has only the permissions below.
const (
	IdentityIngestor      Identity = "ingestor"
	IdentityRunnerManager Identity = "runner-manager"
	IdentityAPI           Identity = "api"
	IdentityTests         Identity = "tests"
)

// Permissions is an allow-list: what an identity may publish and subscribe to.
// Anything not listed is refused by the server.
type Permissions struct {
	Publish   []string
	Subscribe []string
}

// ServicePermissions is every service identity's allow-list.
//
// Written before configuring, as the spec required, and kept as narrow as the
// JetStream client allows:
//
//   - the **ingestor** manages and consumes the one stream: the JetStream API
//     calls for that stream and its consumers, acknowledgements on that
//     stream, and its reply inboxes. It publishes nothing to a session
//     subject — it reads events, it never writes them.
//   - the **runner manager** reads the stream's information and nothing else:
//     M5.5b's drain check needs per-subject message counts. It mints runner
//     credentials; it does not publish events or read them.
//   - the **API** holds a connection only for its readiness probe, which is a
//     protocol-level PING and needs no subject at all. It gets none.
//   - **tests** have their own identity, broad because integration tests
//     create and delete streams of their own, and deliberately *not* any
//     service's, so a leaked test credential is never a production one.
var ServicePermissions = map[Identity]Permissions{
	IdentityIngestor:      IngestorPermissions(StreamName),
	IdentityRunnerManager: RunnerManagerPermissions(StreamName),
	IdentityAPI:           {},
	IdentityTests: {
		Publish:   []string{">"},
		Subscribe: []string{">"},
	},
}

// IngestorPermissions is the ingestor's allow-list for one stream: the
// JetStream API calls for that stream and its consumers, acknowledgements on
// it, and reply inboxes. A function of the stream name so a test can mint the
// same allow-list for a stream of its own and prove it is sufficient.
func IngestorPermissions(stream string) Permissions {
	return Permissions{
		Publish: []string{
			"$JS.API.INFO",
			"$JS.API.STREAM.INFO." + stream,
			"$JS.API.STREAM.CREATE." + stream,
			"$JS.API.CONSUMER.INFO." + stream + ".>",
			"$JS.API.CONSUMER.CREATE." + stream + ".>",
			"$JS.API.CONSUMER.DURABLE.CREATE." + stream + ".>",
			"$JS.API.CONSUMER.MSG.NEXT." + stream + ".>",
			"$JS.ACK." + stream + ".>",
		},
		Subscribe: []string{"_INBOX.>"},
	}
}

// RunnerManagerPermissions is the runner manager's allow-list: the stream's
// information, for M5.5b's drain check, and nothing else.
func RunnerManagerPermissions(stream string) Permissions {
	return Permissions{
		Publish:   []string{"$JS.API.INFO", "$JS.API.STREAM.INFO." + stream},
		Subscribe: []string{"_INBOX.>"},
	}
}

// ServiceCredentials mints a credential for a service identity with the given
// allow-list, signed with the account signing key. Generate uses it for the
// local setup; tests use it to mint an allow-list against a stream of their
// own.
func (i *Issuer) ServiceCredentials(identity Identity, permissions Permissions) (string, error) {
	return serviceCreds(i.signer, i.account, identity, permissions)
}

// RunnerInboxPrefix is the reply-inbox prefix a runner uses, and the only
// thing it may subscribe to.
//
// JetStream acknowledges a publish on a reply subject, so a runner allowed to
// subscribe to nothing could never confirm one. The default `_INBOX.>` would
// let it read every other client's replies; a prefix unique to the runner
// lets it read only its own.
func RunnerInboxPrefix(runnerID uuid.UUID) string {
	return "_INBOX_" + strings.ReplaceAll(runnerID.String(), "-", "")
}

// Issuer mints runner credentials with the application account's signing key.
type Issuer struct {
	signer        nkeys.KeyPair
	account       string
	subjectPrefix string
	lifetime      time.Duration
	now           func() time.Time
}

var _ application.RunnerCredentialIssuer = (*Issuer)(nil)

// NewIssuer builds an issuer from the signing key's seed and the account's
// public key. The seed is a secret: read from a file, never logged.
func NewIssuer(signingSeed []byte, accountPublicKey, subjectPrefix string, now func() time.Time) (*Issuer, error) {
	signer, err := nkeys.FromSeed(signingSeed)
	if err != nil {
		return nil, errors.New("natsauth: the signing key is not a valid seed")
	}
	// A seed of the wrong kind — an operator's, a user's — is a configuration
	// mistake worth naming here rather than failing obscurely at the server.
	if public, err := signer.PublicKey(); err != nil || !nkeys.IsValidPublicAccountKey(public) {
		return nil, errors.New("natsauth: the signing key is not an account signing key")
	}
	if !nkeys.IsValidPublicAccountKey(accountPublicKey) {
		return nil, errors.New("natsauth: the account public key is not valid")
	}
	return &Issuer{
		signer: signer, account: accountPublicKey, subjectPrefix: subjectPrefix,
		lifetime: RunnerCredentialLifetime, now: now,
	}, nil
}

// LoadIssuer reads the signing seed and account public key from files.
func LoadIssuer(signingSeedPath, accountPublicKeyPath, subjectPrefix string) (*Issuer, error) {
	seed, err := os.ReadFile(signingSeedPath) // #nosec G304 -- operator-supplied path
	if err != nil {
		return nil, fmt.Errorf("natsauth: read signing key: %w", err)
	}
	account, err := os.ReadFile(accountPublicKeyPath) // #nosec G304 -- operator-supplied path
	if err != nil {
		return nil, fmt.Errorf("natsauth: read account public key: %w", err)
	}
	return NewIssuer([]byte(strings.TrimSpace(string(seed))), strings.TrimSpace(string(account)), subjectPrefix, time.Now)
}

// IssueRunner mints a credential for one runner of one session.
//
// Publish: that session's event subject, and nothing else. Subscribe: the
// runner's own reply inbox, and nothing else. Named for the runner, so a
// server log line names it — and nothing in it names a customer, a workspace
// or a repository.
func (i *Issuer) IssueRunner(runnerID, sessionID uuid.UUID) (application.RunnerCredentials, error) {
	user, err := nkeys.CreateUser()
	if err != nil {
		return application.RunnerCredentials{}, fmt.Errorf("natsauth: create runner key: %w", err)
	}
	public, err := user.PublicKey()
	if err != nil {
		return application.RunnerCredentials{}, err
	}
	seed, err := user.Seed()
	if err != nil {
		return application.RunnerCredentials{}, err
	}

	subject := domain.EventSubject(i.subjectPrefix, sessionID)
	inbox := RunnerInboxPrefix(runnerID)
	expires := i.now().Add(i.lifetime)

	claims := jwt.NewUserClaims(public)
	claims.Name = "runner-" + runnerID.String()
	claims.IssuerAccount = i.account
	claims.Expires = expires.Unix()
	claims.Pub.Allow.Add(subject)
	claims.Sub.Allow.Add(inbox + ".>")
	token, err := claims.Encode(i.signer)
	if err != nil {
		return application.RunnerCredentials{}, fmt.Errorf("natsauth: sign runner credential: %w", err)
	}
	creds, err := jwt.FormatUserConfig(token, seed)
	if err != nil {
		return application.RunnerCredentials{}, fmt.Errorf("natsauth: format runner credential: %w", err)
	}
	return application.RunnerCredentials{
		Creds: string(creds), Subject: subject, InboxPrefix: inbox, ExpiresAt: expires,
	}, nil
}

// Setup is a generated set of local credentials. Files, not values: the
// seeds in them are secrets and are written with owner-only permissions.
type Setup struct {
	Dir string
}

// Paths within a generated setup.
const (
	ServerConfigFile   = "server.conf"
	RunnerSigningFile  = "runner-signing.nk"
	AccountPublicFile  = "account.pub"
	credsFileExtension = ".creds"
)

// CredsFile is where an identity's credentials live in a generated setup.
func (s Setup) CredsFile(identity Identity) string {
	return filepath.Join(s.Dir, string(identity)+credsFileExtension)
}

// Generate creates an operator, a system account, the application account
// with a separate signing key, the server configuration that trusts them, and
// credentials for every service identity.
//
// For local development and tests. Production keys are created and held in a
// secret store (M9); the model is the same.
//
// **All or nothing.** Everything is written into a temporary sibling
// directory and renamed into place in one step, so an interrupted run leaves
// no setup rather than a partial one. The first version wrote the server
// configuration first and the credentials after; a run that stopped between
// them left a directory that looked complete to Complete and could never be
// repaired by re-running.
func Generate(dir string) (Setup, error) {
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return Setup{}, err
	}
	staging, err := os.MkdirTemp(parent, "."+filepath.Base(dir)+"-")
	if err != nil {
		return Setup{}, err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	if err := generateInto(staging); err != nil {
		return Setup{}, err
	}
	if err := os.Chmod(staging, 0o700); err != nil {
		return Setup{}, err
	}
	// Replace whatever was there — an incomplete setup, or one being
	// regenerated with -force — only once the new one is whole.
	if err := os.RemoveAll(dir); err != nil {
		return Setup{}, err
	}
	if err := os.Rename(staging, dir); err != nil {
		return Setup{}, err
	}
	return Setup{Dir: dir}, nil
}

// Complete reports whether dir holds every file a setup needs. A setup
// missing any of them is regenerated rather than trusted.
func Complete(dir string) bool {
	setup := Setup{Dir: dir}
	required := []string{
		filepath.Join(dir, ServerConfigFile),
		filepath.Join(dir, RunnerSigningFile),
		filepath.Join(dir, AccountPublicFile),
	}
	for identity := range ServicePermissions {
		required = append(required, setup.CredsFile(identity))
	}
	for _, path := range required {
		if info, err := os.Stat(path); err != nil || info.Size() == 0 || !info.Mode().IsRegular() {
			return false
		}
	}
	return true
}

func generateInto(dir string) error {
	operator, err := nkeys.CreateOperator()
	if err != nil {
		return err
	}
	system, err := nkeys.CreateAccount()
	if err != nil {
		return err
	}
	account, err := nkeys.CreateAccount()
	if err != nil {
		return err
	}
	signing, err := nkeys.CreateAccount()
	if err != nil {
		return err
	}
	operatorPub, _ := operator.PublicKey()
	systemPub, _ := system.PublicKey()
	accountPub, _ := account.PublicKey()
	signingPub, _ := signing.PublicKey()

	operatorClaims := jwt.NewOperatorClaims(operatorPub)
	operatorClaims.Name = "weave"
	operatorClaims.SystemAccount = systemPub
	operatorJWT, err := operatorClaims.Encode(operator)
	if err != nil {
		return fmt.Errorf("sign operator: %w", err)
	}

	systemClaims := jwt.NewAccountClaims(systemPub)
	systemClaims.Name = "SYS"
	systemJWT, err := systemClaims.Encode(operator)
	if err != nil {
		return fmt.Errorf("sign system account: %w", err)
	}

	accountClaims := jwt.NewAccountClaims(accountPub)
	accountClaims.Name = "WEAVE"
	// JetStream is disabled in an account unless given limits. Unlimited
	// here; the stream's own limits (M5.3) are what bound it.
	accountClaims.Limits.JetStreamLimits = jwt.JetStreamLimits{
		MemoryStorage: jwt.NoLimit, DiskStorage: jwt.NoLimit, Streams: jwt.NoLimit, Consumer: jwt.NoLimit,
	}
	accountClaims.SigningKeys.Add(signingPub)
	accountJWT, err := accountClaims.Encode(operator)
	if err != nil {
		return fmt.Errorf("sign application account: %w", err)
	}

	config := fmt.Sprintf(`# Generated by services/nats-setup. Local development only; do not commit.
operator: %s
system_account: %s
resolver: MEMORY
resolver_preload: {
  %s: %s
  %s: %s
}
`, operatorJWT, systemPub, systemPub, systemJWT, accountPub, accountJWT)

	setup := Setup{Dir: dir}
	if err := writeFile(filepath.Join(dir, ServerConfigFile), config, 0o644); err != nil {
		return err
	}
	signingSeed, _ := signing.Seed()
	if err := writeFile(filepath.Join(dir, RunnerSigningFile), string(signingSeed)+"\n", 0o600); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(dir, AccountPublicFile), accountPub+"\n", 0o644); err != nil {
		return err
	}

	for identity, permissions := range ServicePermissions {
		creds, err := serviceCreds(signing, accountPub, identity, permissions)
		if err != nil {
			return err
		}
		if err := writeFile(setup.CredsFile(identity), creds, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// serviceCreds mints a long-lived credential for a service identity.
//
// An identity with no permissions gets explicit deny-all rather than an empty
// allow-list, which the server would read as "no restriction".
func serviceCreds(signing nkeys.KeyPair, accountPub string, identity Identity, permissions Permissions) (string, error) {
	user, err := nkeys.CreateUser()
	if err != nil {
		return "", err
	}
	public, _ := user.PublicKey()
	seed, _ := user.Seed()

	claims := jwt.NewUserClaims(public)
	claims.Name = string(identity)
	claims.IssuerAccount = accountPub
	if len(permissions.Publish) == 0 {
		claims.Pub.Deny.Add(">")
	} else {
		claims.Pub.Allow.Add(permissions.Publish...)
	}
	if len(permissions.Subscribe) == 0 {
		claims.Sub.Deny.Add(">")
	} else {
		claims.Sub.Allow.Add(permissions.Subscribe...)
	}
	token, err := claims.Encode(signing)
	if err != nil {
		return "", fmt.Errorf("sign %s credential: %w", identity, err)
	}
	creds, err := jwt.FormatUserConfig(token, seed)
	if err != nil {
		return "", err
	}
	return string(creds), nil
}

func writeFile(path, contents string, mode os.FileMode) error {
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	return os.Chmod(path, mode)
}

// DefaultDir is where `make nats-auth` generates local credentials, relative
// to the repository root the processes run from.
const DefaultDir = "infra/nats/generated"

// ErrCredentialsRequired means a NATS credential was not configured outside
// development and test.
var ErrCredentialsRequired = errors.New("a NATS credential is required outside development and test")

// ResolvePath returns the configured path, or — in development and test only —
// the generated default for the identity. Anywhere else an unset credential is
// an error: a deployment that forgot it must fail to start, not connect
// unauthenticated or with a developer's key.
func ResolvePath(appEnv, configured, generatedFile string) (string, error) {
	if configured != "" {
		return configured, nil
	}
	switch strings.ToLower(strings.TrimSpace(appEnv)) {
	case "development", "test":
		return filepath.Join(DefaultDir, generatedFile), nil
	default:
		return "", ErrCredentialsRequired
	}
}
