// Package devdocker runs session runners as local Docker containers, for
// development.
//
// **This is not a security boundary.** ADR-013 puts untrusted code on a
// managed microVM or gVisor provider; a container on a developer's laptop
// shares the host kernel, and this backend does not enforce the egress policy.
// It is hardened anyway — non-root, every capability dropped, read-only root,
// no host mounts, no Docker socket inside, resource limits — so that
// development exercises the same shape production will. The name says what it
// is, and New refuses to build one when the environment is staging or
// production, so it cannot be deployed by accident.
//
// It speaks the Docker Engine API directly over the unix socket rather than
// through the Docker SDK: the calls are few and plain, and the SDK would pull
// a large dependency tree into go.mod for a development-only adapter.
package devdocker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/application"
)

// BackendName is stored on every runner this backend creates.
const BackendName = "dev-docker"

// ErrRefusedOutsideDevelopment is returned by New in staging and production.
var ErrRefusedOutsideDevelopment = errors.New(
	"the dev Docker runner backend is not a security boundary and is refused outside development")

// ErrImageMissing means the runner image has not been built.
var ErrImageMissing = errors.New("the runner image is not built; run `make runner-image`")

// Labels mark what this backend owns, so it never touches a container it did
// not create — and so a test's scope and a developer's `make dev` scope never
// see each other's runners. M5.2's review found a dev worker claiming test
// work; the scope label designs that out here.
const (
	labelManaged = "dev.weave.managed"
	labelScope   = "dev.weave.scope"
	labelRunner  = "dev.weave.runner-id"
	labelSession = "dev.weave.session-id"
)

// Config sets up the backend.
type Config struct {
	// AppEnv must be a development or test environment.
	AppEnv string
	// Socket is the Docker socket path.
	Socket string
	// Image is the runner image, built by `make runner-image`.
	Image string
	// Scope separates runner sets: "dev" for `make dev`, a unique value per
	// test.
	Scope string
}

// Backend implements application.RunnerBackend with local Docker.
type Backend struct {
	http  *http.Client
	image string
	scope string
}

var _ application.RunnerBackend = (*Backend)(nil)

// New builds the backend, refusing staging and production.
func New(cfg Config) (*Backend, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.AppEnv)) {
	case "staging", "production":
		return nil, ErrRefusedOutsideDevelopment
	}
	if cfg.Socket == "" || cfg.Image == "" || cfg.Scope == "" {
		return nil, errors.New("devdocker: socket, image and scope are required")
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", cfg.Socket)
		},
	}
	return &Backend{
		http:  &http.Client{Transport: transport, Timeout: 60 * time.Second},
		image: cfg.Image,
		scope: cfg.Scope,
	}, nil
}

// Name identifies this backend on runner rows.
func (b *Backend) Name() string { return BackendName }

// name is the container's and its volume's name. Deterministic from the runner
// id, which is what makes Provision idempotent and HandleFor possible.
func (b *Backend) name(runnerID uuid.UUID) string {
	return "weave-runner-" + b.scope + "-" + runnerID.String()
}

// Provision creates and starts the runner's container and its workspace
// volume. A runner id that already has a container gets that container.
func (b *Backend) Provision(ctx context.Context, spec application.RunnerSpec) (string, error) {
	if _, err := b.call(ctx, http.MethodGet, "/images/"+url.PathEscape(b.image)+"/json", nil, http.StatusOK); err != nil {
		if errors.Is(err, errNotFound) {
			return "", ErrImageMissing
		}
		return "", fmt.Errorf("devdocker: inspect runner image: %w", err)
	}

	name := b.name(spec.RunnerID)
	labels := map[string]string{
		labelManaged: "true", labelScope: b.scope,
		labelRunner: spec.RunnerID.String(), labelSession: spec.SessionID.String(),
	}

	// The workspace: an ephemeral volume, created for this runner and removed
	// with it. ADR-013 retains no source after teardown.
	if _, err := b.call(ctx, http.MethodPost, "/volumes/create",
		map[string]any{"Name": name, "Labels": labels}, http.StatusCreated, http.StatusOK); err != nil {
		return "", fmt.Errorf("devdocker: create workspace volume: %w", err)
	}

	container := map[string]any{
		"Image": b.image,
		// The UID the image's /workspace is owned by; never root.
		"User":       "10001:10001",
		"WorkingDir": "/workspace",
		"Labels":     labels,
		// Environment is visible to `docker inspect` on the host. Acceptable
		// for a development backend on the developer's own machine; ADR-013
		// requires the production provider to inject secrets without that.
		"Env": []string{
			"WEAVE_RUNNER_ID=" + spec.RunnerID.String(),
			"WEAVE_SESSION_ID=" + spec.SessionID.String(),
			"WEAVE_CLONE_URL=" + spec.CloneURL,
			"WEAVE_BRANCH=" + spec.Branch,
			"WEAVE_COMMIT=" + spec.Commit,
			"WEAVE_GIT_TOKEN=" + spec.GitToken,
			"HOME=/tmp",
		},
		// Readiness is the runner's own check that its checkout completed.
		"Healthcheck": map[string]any{
			"Test":     []string{"CMD", "/usr/local/bin/weave-runner", "healthcheck"},
			"Interval": int64(time.Second), "Timeout": int64(2 * time.Second), "Retries": 1,
		},
		"HostConfig": map[string]any{
			"ReadonlyRootfs": true,
			"CapDrop":        []string{"ALL"},
			"SecurityOpt":    []string{"no-new-privileges:true"},
			"Privileged":     false,
			"Tmpfs":          map[string]string{"/tmp": "rw,nosuid,nodev,size=64m"},
			"Mounts": []map[string]any{
				{"Type": "volume", "Source": name, "Target": "/workspace"},
			},
			"Memory":    int64(2 << 30),
			"NanoCpus":  int64(2e9),
			"PidsLimit": int64(512),
			// For reaching a local git server in tests and development.
			"ExtraHosts": []string{"host.docker.internal:host-gateway"},
		},
	}
	if _, err := b.call(ctx, http.MethodPost, "/containers/create?name="+url.QueryEscape(name),
		container, http.StatusCreated); err != nil && !errors.Is(err, errConflict) {
		return "", fmt.Errorf("devdocker: create runner container: %w", err)
	}
	if _, err := b.call(ctx, http.MethodPost, "/containers/"+name+"/start", nil,
		http.StatusNoContent, http.StatusNotModified); err != nil {
		return "", fmt.Errorf("devdocker: start runner container: %w", err)
	}
	return name, nil
}

// Status reads the container's state and health.
func (b *Backend) Status(ctx context.Context, handle string) (application.RunnerStatus, error) {
	body, err := b.call(ctx, http.MethodGet, "/containers/"+url.PathEscape(handle)+"/json", nil, http.StatusOK)
	if errors.Is(err, errNotFound) {
		return application.RunnerStatus{Exists: false}, nil
	}
	if err != nil {
		return application.RunnerStatus{}, fmt.Errorf("devdocker: inspect runner: %w", err)
	}
	var inspect struct {
		State struct {
			Running  bool
			ExitCode int
			Health   *struct{ Status string }
		}
	}
	if err := json.Unmarshal(body, &inspect); err != nil {
		return application.RunnerStatus{}, fmt.Errorf("devdocker: decode inspect: %w", err)
	}
	return application.RunnerStatus{
		Exists:   true,
		Running:  inspect.State.Running,
		Ready:    inspect.State.Health != nil && inspect.State.Health.Status == "healthy",
		ExitCode: inspect.State.ExitCode,
	}, nil
}

// Destroy removes the container and its workspace volume. Either already gone
// is success.
func (b *Backend) Destroy(ctx context.Context, handle string) error {
	if _, err := b.call(ctx, http.MethodDelete, "/containers/"+url.PathEscape(handle)+"?force=true",
		nil, http.StatusNoContent); err != nil && !errors.Is(err, errNotFound) {
		return fmt.Errorf("devdocker: remove runner container: %w", err)
	}
	// The volume can report "in use" for a moment after its container goes.
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		_, err = b.call(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(handle), nil, http.StatusNoContent)
		if err == nil || errors.Is(err, errNotFound) {
			return nil
		}
		if !errors.Is(err, errConflict) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("devdocker: remove workspace volume: %w", err)
}

// HandleFor finds a runner's container or volume by runner id.
func (b *Backend) HandleFor(ctx context.Context, runnerID uuid.UUID) (string, bool, error) {
	name := b.name(runnerID)
	if _, err := b.call(ctx, http.MethodGet, "/containers/"+name+"/json", nil, http.StatusOK); err == nil {
		return name, true, nil
	} else if !errors.Is(err, errNotFound) {
		return "", false, err
	}
	if _, err := b.call(ctx, http.MethodGet, "/volumes/"+name, nil, http.StatusOK); err == nil {
		return name, true, nil
	} else if !errors.Is(err, errNotFound) {
		return "", false, err
	}
	return "", false, nil
}

// List returns every runner environment in this scope — containers, and
// volumes whose container is already gone.
func (b *Backend) List(ctx context.Context) ([]application.BackendRunner, error) {
	filter, _ := json.Marshal(map[string][]string{"label": {labelManaged + "=true", labelScope + "=" + b.scope}})
	seen := map[uuid.UUID]application.BackendRunner{}

	body, err := b.call(ctx, http.MethodGet, "/containers/json?all=true&filters="+url.QueryEscape(string(filter)), nil, http.StatusOK)
	if err != nil {
		return nil, fmt.Errorf("devdocker: list containers: %w", err)
	}
	var containers []struct {
		Names  []string
		Labels map[string]string
	}
	if err := json.Unmarshal(body, &containers); err != nil {
		return nil, fmt.Errorf("devdocker: decode containers: %w", err)
	}
	for _, c := range containers {
		if id, err := uuid.Parse(c.Labels[labelRunner]); err == nil {
			seen[id] = application.BackendRunner{RunnerID: id, Handle: b.name(id)}
		}
	}

	body, err = b.call(ctx, http.MethodGet, "/volumes?filters="+url.QueryEscape(string(filter)), nil, http.StatusOK)
	if err != nil {
		return nil, fmt.Errorf("devdocker: list volumes: %w", err)
	}
	var volumes struct {
		Volumes []struct{ Labels map[string]string }
	}
	if err := json.Unmarshal(body, &volumes); err != nil {
		return nil, fmt.Errorf("devdocker: decode volumes: %w", err)
	}
	for _, v := range volumes.Volumes {
		if id, err := uuid.Parse(v.Labels[labelRunner]); err == nil {
			seen[id] = application.BackendRunner{RunnerID: id, Handle: b.name(id)}
		}
	}

	out := make([]application.BackendRunner, 0, len(seen))
	for _, runner := range seen {
		out = append(out, runner)
	}
	return out, nil
}

var (
	errNotFound = errors.New("not found")
	errConflict = errors.New("conflict")
)

// call makes one Docker Engine API request, accepting the listed statuses.
func (b *Backend) call(ctx context.Context, method, path string, body any, accept ...int) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	for _, status := range accept {
		if resp.StatusCode == status {
			return payload, nil
		}
	}
	switch resp.StatusCode {
	case http.StatusNotFound:
		return nil, errNotFound
	case http.StatusConflict:
		return nil, errConflict
	}
	return nil, fmt.Errorf("docker %s %s returned %d: %s", method, strings.SplitN(path, "?", 2)[0],
		resp.StatusCode, strings.TrimSpace(string(payload[:min(len(payload), 300)])))
}
