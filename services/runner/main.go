// Command weave-runner runs inside a session's sandbox.
//
// In M5.4a it does one thing: check out the session's branch at exactly the
// commit the control plane recorded, report ready, and wait to be torn down.
// It emits no session events and holds no NATS credentials — events come from
// a provider (M5.5 on), and a runner's scoped credentials arrive with them.
//
// It runs as an unprivileged user on a read-only root filesystem with every
// capability dropped. It trusts nothing in the repository: `git fetch` and
// `git checkout` run no repository code, and nothing here reads a file the
// repository supplied.
package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
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
)

func main() {
	// Before anything else: move the git token out of the environment the
	// kernel remembers. See scrubToken.
	if err := scrubToken(); err != nil {
		fmt.Fprintf(os.Stderr, "weave-runner: %v\n", err)
		os.Exit(exitMisconfigured)
	}
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		// Ready means the checkout completed and was verified.
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

	if err := os.WriteFile(readyFile, []byte("ready\n"), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "weave-runner: mark ready: %v\n", err)
		return exitMisconfigured
	}
	fmt.Fprintln(os.Stderr, "weave-runner: checkout verified; ready")

	// Nothing to do until a provider exists. Wait to be torn down.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, os.Interrupt)
	<-stop
	return 0
}

type config struct {
	cloneURL string
	branch   string
	commit   string
	token    string
}

// tokenFDEnv names the inherited descriptor the token arrives on after
// scrubToken's re-exec.
const tokenFDEnv = "WEAVE_GIT_TOKEN_FD"

// scrubToken moves the git token out of this process's initial environment.
//
// os.Unsetenv is not enough, and the first version relied on it: it changes
// Go's copy of the environment, while /proc/<pid>/environ — fixed when the
// process was exec'd — still holds the token, readable by any process running
// as the same user. From M5.5 that includes a provider running model-driven
// commands. So the token is written into a pipe, the read end is kept open
// across exec, and the runner re-executes itself with an environment that
// does not contain it. The new image's /proc/<pid>/environ never has it.
func scrubToken() error {
	token, present := os.LookupEnv("WEAVE_GIT_TOKEN")
	if !present {
		return nil
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("scrub token: %w", err)
	}
	if _, err := writer.WriteString(token); err != nil {
		return fmt.Errorf("scrub token: %w", err)
	}
	_ = writer.Close()
	// os.Pipe marks descriptors close-on-exec; the read end must survive.
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, reader.Fd(), syscall.F_SETFD, 0); errno != 0 {
		return fmt.Errorf("scrub token: keep descriptor: %v", errno)
	}

	env := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "WEAVE_GIT_TOKEN=") {
			env = append(env, kv)
		}
	}
	env = append(env, fmt.Sprintf("%s=%d", tokenFDEnv, reader.Fd()))

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("scrub token: %w", err)
	}
	return syscall.Exec(self, os.Args, env)
}

// readToken takes the token from the descriptor scrubToken left open, and
// closes it.
func readToken() string {
	raw := os.Getenv(tokenFDEnv)
	if raw == "" {
		return ""
	}
	_ = os.Unsetenv(tokenFDEnv)
	fd, err := strconv.Atoi(raw)
	if err != nil {
		return ""
	}
	file := os.NewFile(uintptr(fd), "git-token")
	defer func() { _ = file.Close() }()
	token, err := io.ReadAll(io.LimitReader(file, 4096))
	if err != nil {
		return ""
	}
	return string(token)
}

// readConfig takes what the control plane delivered at start.
func readConfig() (config, error) {
	cfg := config{
		cloneURL: os.Getenv("WEAVE_CLONE_URL"),
		branch:   os.Getenv("WEAVE_BRANCH"),
		commit:   os.Getenv("WEAVE_COMMIT"),
		token:    readToken(),
	}

	var missing []string
	for name, value := range map[string]string{
		"WEAVE_CLONE_URL": cfg.cloneURL, "WEAVE_BRANCH": cfg.branch, "WEAVE_COMMIT": cfg.commit,
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
	return cfg, nil
}

// checkout fetches exactly the recorded commit and verifies HEAD is it.
//
// Fetched by commit id, not by branch name: the branch can move between the
// control plane recording it and this fetch, and working on whatever it now
// points at would be working on code nobody chose. A checkout that is not at
// the recorded commit exits with a distinct code rather than proceeding.
func checkout(cfg config) int {
	env := gitEnv(cfg.token)

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
		// No system or global config from the image to surprise anyone.
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
