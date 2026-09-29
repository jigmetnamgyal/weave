// Command weave-runner runs inside a session's sandbox.
//
// It checks out the session's branch at exactly the commit the control plane
// recorded (M5.4a), connects to the event broker with a credential scoped to
// its own session and proves that scope holds (M5.5a), reports ready, and
// waits to be torn down. It emits no session events yet — events come from a
// provider, which is M5.5b.
//
// It runs as an unprivileged user on a read-only root filesystem with every
// capability dropped. It trusts nothing in the repository: `git fetch` and
// `git checkout` run no repository code, and nothing here reads a file the
// repository supplied.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	readyFile   = "/tmp/weave-ready"
	checkoutDir = "/workspace/repo"

	// Exit codes the control plane reads through the backend's status. The
	// only thing it reads from a runner, so the runner cannot talk it into
	// anything. Matched by application.RunnerExit*.
	exitCloneFailed      = 2
	exitCheckoutMismatch = 3
	exitMisconfigured    = 4
	exitBrokerRefused    = 5
)

// secretEnv are the variables that carry secrets at start. scrubSecrets moves
// all of them out of the process's initial environment before anything runs.
var secretEnv = []string{"WEAVE_GIT_TOKEN", "WEAVE_NATS_CREDS"}

func main() {
	if err := scrubSecrets(); err != nil {
		fmt.Fprintf(os.Stderr, "weave-runner: %v\n", err)
		os.Exit(exitMisconfigured)
	}
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		// Ready means the checkout was verified and the broker credential
		// proved its scope.
		if _, err := os.Stat(readyFile); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(run())
}

func run() int {
	cfg, err := readConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "weave-runner: %v\n", err)
		return exitMisconfigured
	}

	if code := checkout(cfg); code != 0 {
		return code
	}

	conn, err := connectBroker(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "weave-runner: event broker: %v\n", err)
		return exitBrokerRefused
	}
	// Held open for the provider (M5.5b); closed at teardown.
	defer conn.Close()

	if err := os.WriteFile(readyFile, []byte("ready\n"), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "weave-runner: mark ready: %v\n", err)
		return exitMisconfigured
	}
	fmt.Fprintln(os.Stderr, "weave-runner: checkout verified; broker credential verified; ready")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, os.Interrupt)
	<-stop
	return 0
}

type secrets struct {
	GitToken  string `json:"git_token"`
	NATSCreds string `json:"nats_creds"`
}

type config struct {
	sessionID string
	cloneURL  string
	branch    string
	commit    string
	natsURL   string
	subject   string
	inbox     string
	secrets   secrets
}

// secretsFDEnv names the inherited descriptor the secrets arrive on after
// scrubSecrets' re-exec.
const secretsFDEnv = "WEAVE_SECRETS_FD"

// scrubSecrets moves every secret out of this process's initial environment.
//
// os.Unsetenv is not enough — M5.4a found that with a test. It changes Go's
// copy of the environment, while /proc/<pid>/environ, fixed when the process
// was exec'd, still holds the value, readable by any process running as the
// same user. So the secrets are written into a pipe as one JSON document, the
// read end is kept open across exec, and the runner re-executes itself with
// an environment that contains none of them.
func scrubSecrets() error {
	var found secrets
	present := false
	if v, ok := os.LookupEnv("WEAVE_GIT_TOKEN"); ok {
		found.GitToken, present = v, true
	}
	if v, ok := os.LookupEnv("WEAVE_NATS_CREDS"); ok {
		found.NATSCreds, present = v, true
	}
	if !present {
		return nil
	}

	payload, err := json.Marshal(found)
	if err != nil {
		return fmt.Errorf("scrub secrets: %w", err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("scrub secrets: %w", err)
	}
	if _, err := writer.Write(payload); err != nil {
		return fmt.Errorf("scrub secrets: %w", err)
	}
	_ = writer.Close()
	// os.Pipe marks descriptors close-on-exec; the read end must survive.
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, reader.Fd(), syscall.F_SETFD, 0); errno != 0 {
		return fmt.Errorf("scrub secrets: keep descriptor: %v", errno)
	}

	env := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		secret := false
		for _, name := range secretEnv {
			if strings.HasPrefix(kv, name+"=") {
				secret = true
			}
		}
		if !secret {
			env = append(env, kv)
		}
	}
	env = append(env, fmt.Sprintf("%s=%d", secretsFDEnv, reader.Fd()))

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("scrub secrets: %w", err)
	}
	return syscall.Exec(self, os.Args, env)
}

// readSecrets takes the secrets from the descriptor scrubSecrets left open.
func readSecrets() secrets {
	raw := os.Getenv(secretsFDEnv)
	if raw == "" {
		return secrets{}
	}
	_ = os.Unsetenv(secretsFDEnv)
	fd, err := strconv.Atoi(raw)
	if err != nil {
		return secrets{}
	}
	file := os.NewFile(uintptr(fd), "secrets")
	defer func() { _ = file.Close() }()
	payload, err := io.ReadAll(io.LimitReader(file, 64<<10))
	if err != nil {
		return secrets{}
	}
	var found secrets
	_ = json.Unmarshal(payload, &found)
	return found
}

// readConfig takes what the control plane delivered at start.
func readConfig() (config, error) {
	cfg := config{
		sessionID: os.Getenv("WEAVE_SESSION_ID"),
		cloneURL:  os.Getenv("WEAVE_CLONE_URL"),
		branch:    os.Getenv("WEAVE_BRANCH"),
		commit:    os.Getenv("WEAVE_COMMIT"),
		natsURL:   os.Getenv("WEAVE_NATS_URL"),
		subject:   os.Getenv("WEAVE_EVENT_SUBJECT"),
		inbox:     os.Getenv("WEAVE_NATS_INBOX"),
		secrets:   readSecrets(),
	}

	var missing []string
	for name, value := range map[string]string{
		"WEAVE_SESSION_ID": cfg.sessionID, "WEAVE_CLONE_URL": cfg.cloneURL, "WEAVE_BRANCH": cfg.branch,
		"WEAVE_COMMIT": cfg.commit, "WEAVE_NATS_URL": cfg.natsURL, "WEAVE_EVENT_SUBJECT": cfg.subject,
		"WEAVE_NATS_INBOX": cfg.inbox, "WEAVE_NATS_CREDS": cfg.secrets.NATSCreds,
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return config{}, fmt.Errorf("missing %s", strings.Join(missing, ", "))
	}
	if len(cfg.commit) != 40 && len(cfg.commit) != 64 {
		return config{}, errors.New("WEAVE_COMMIT is not a full commit id")
	}
	if !strings.Contains(cfg.subject, cfg.sessionID) {
		return config{}, errors.New("WEAVE_EVENT_SUBJECT does not name this session")
	}
	return cfg, nil
}

// connectBroker connects with the runner's scoped credential and proves the
// scope holds, in both directions, before the runner calls itself ready.
//
//  1. **Its own subject is publishable**, through JetStream, with nothing
//     stored: the publish carries an expected per-subject sequence no subject
//     can have, so the stream refuses it with "wrong last sequence". That
//     answer can only come back if the publish was permitted, reached the
//     stream, and the reply reached the runner's own inbox — the three
//     things a real publish needs — while writing nothing to history.
//  2. **Another session's subject is refused** by the broker. A credential
//     that could publish there would be the gap M5.3 handed forward.
//
// A runner whose credential fails either check exits rather than becoming
// ready and failing later, or — worse — succeeding where it should not.
func connectBroker(cfg config) (*nats.Conn, error) {
	userJWT, err := jwt.ParseDecoratedJWT([]byte(cfg.secrets.NATSCreds))
	if err != nil {
		return nil, errors.New("the credential is not a valid NATS credential")
	}
	key, err := jwt.ParseDecoratedUserNKey([]byte(cfg.secrets.NATSCreds))
	if err != nil {
		return nil, errors.New("the credential carries no valid key")
	}
	seed, err := key.Seed()
	if err != nil {
		return nil, errors.New("the credential carries no valid key")
	}

	violations := make(chan error, 8)
	conn, err := nats.Connect(cfg.natsURL,
		nats.Name("weave-runner"),
		nats.UserJWTAndSeed(userJWT, string(seed)),
		nats.CustomInboxPrefix(cfg.inbox),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			select {
			case violations <- err:
			default:
			}
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}

	js, err := jetstream.New(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = js.Publish(ctx, cfg.subject, []byte("{}"),
		jetstream.WithExpectLastSequencePerSubject(1<<62))
	// Replicated streams report the same refusal under a second code; both
	// mean the publish was permitted and reached the stream.
	var apiErr *jetstream.APIError
	if !errors.As(err, &apiErr) || (apiErr.ErrorCode != jetstream.JSErrCodeStreamWrongLastSequence &&
		apiErr.ErrorCode != jetstream.JSErrCodeStreamWrongLastSequenceConstant) {
		conn.Close()
		return nil, fmt.Errorf("its own subject is not publishable as it must be: %v", err)
	}

	neighbour := strings.Replace(cfg.subject, cfg.sessionID, uuid.NewString(), 1)
	if err := conn.Publish(neighbour, []byte("{}")); err != nil {
		conn.Close()
		return nil, err
	}
	if err := conn.FlushTimeout(5 * time.Second); err != nil {
		conn.Close()
		return nil, err
	}
	select {
	case violation := <-violations:
		if !errors.Is(violation, nats.ErrPermissionViolation) &&
			!strings.Contains(strings.ToLower(violation.Error()), "permissions violation") {
			conn.Close()
			return nil, fmt.Errorf("an unexpected broker error: %v", violation)
		}
	case <-time.After(5 * time.Second):
		conn.Close()
		return nil, errors.New("another session's subject was not refused")
	}
	return conn, nil
}

// checkout fetches exactly the recorded commit and verifies HEAD is it.
//
// Fetched by commit id, not by branch name: the branch can move between the
// control plane recording it and this fetch, and working on whatever it now
// points at would be working on code nobody chose. A checkout that is not at
// the recorded commit exits with a distinct code rather than proceeding.
func checkout(cfg config) int {
	env := gitEnv(cfg.secrets.GitToken)

	steps := [][]string{
		{"init", "-q", checkoutDir},
		{"-C", checkoutDir, "fetch", "-q", "--depth", "1", "--no-tags", cfg.cloneURL, cfg.commit},
		{"-C", checkoutDir, "checkout", "-q", "--detach", "FETCH_HEAD"},
	}
	for _, args := range steps {
		if err := git(env, args...); err != nil {
			// The error names the git step, never the token: it travels in
			// the environment, not the arguments.
			fmt.Fprintf(os.Stderr, "weave-runner: git %s failed: %v\n", args[len(args)-2], err)
			return exitCloneFailed
		}
	}

	head, err := exec.Command("git", "-C", checkoutDir, "rev-parse", "HEAD").Output()
	if err != nil {
		return exitCloneFailed
	}
	if strings.TrimSpace(string(head)) != cfg.commit {
		fmt.Fprintln(os.Stderr, "weave-runner: the checkout is not at the recorded commit; refusing to continue")
		return exitCheckoutMismatch
	}

	if err := git(env, "-C", checkoutDir, "switch", "-q", "-c", cfg.branch); err != nil {
		return exitCloneFailed
	}
	return 0
}

// gitEnv authenticates git through configuration in the environment, so the
// token never appears in a process's arguments, and never in .git/config.
func gitEnv(token string) []string {
	env := []string{
		"HOME=/tmp",
		"PATH=" + os.Getenv("PATH"),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
	}
	if token != "" {
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		env = append(env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraHeader",
			"GIT_CONFIG_VALUE_0=Authorization: Basic "+basic,
		)
	}
	return env
}

func git(env []string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Env = env
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
