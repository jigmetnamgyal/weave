package github

import (
	"context"
	"errors"

	"github.com/jigmetnamgyal/weave/internal/application"
)

// Port adapts Client to the application's GitHubAPI port.
//
// A thin translation layer rather than having the application depend on this
// package's types. It exists so the application layer states what it needs
// from GitHub in its own vocabulary, and so a future change in GitHub's
// response shapes stops here.
type Port struct {
	client *Client
}

// NewPort wraps a client.
func NewPort(client *Client) *Port { return &Port{client: client} }

// Installation reads one installation and translates it.
func (p *Port) Installation(ctx context.Context, githubInstallationID int64) (application.RemoteInstallation, error) {
	remote, err := p.client.Installation(ctx, githubInstallationID)
	if err != nil {
		return application.RemoteInstallation{}, err
	}
	return application.RemoteInstallation{
		AccountLogin:        remote.Account.Login,
		AccountType:         remote.Account.Type,
		RepositorySelection: remote.RepositorySelection,
		Permissions:         remote.Permissions,
		SuspendedAt:         remote.SuspendedAt,
	}, nil
}

// InstallationRepositories lists what the installation grants and translates
// it.
func (p *Port) InstallationRepositories(ctx context.Context, githubInstallationID int64) ([]application.RemoteRepository, error) {
	remotes, err := p.client.InstallationRepositories(ctx, githubInstallationID)
	if err != nil {
		return nil, err
	}
	repositories := make([]application.RemoteRepository, 0, len(remotes))
	for _, remote := range remotes {
		repositories = append(repositories, application.RemoteRepository{
			GitHubID:      remote.ID,
			Owner:         remote.Owner.Login,
			Name:          remote.Name,
			DefaultBranch: remote.DefaultBranch,
			Private:       remote.Private,
		})
	}
	return repositories, nil
}

// Branch reads one branch, translating GitHub's not-found into the port's.
//
// The translation matters: the service distinguishes "no such branch" — the
// ordinary answer before creating one — from a failure, and it must do so
// without importing this package's errors.
func (p *Port) Branch(ctx context.Context, githubInstallationID int64, owner, repo, branch string) (application.RemoteBranch, error) {
	state, err := p.client.Branch(ctx, githubInstallationID, owner, repo, branch)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return application.RemoteBranch{}, application.ErrRemoteNotFound
		}
		return application.RemoteBranch{}, err
	}
	return application.RemoteBranch{Name: state.Name, SHA: state.SHA, Protected: state.Protected}, nil
}

// CreateBranch creates a branch, translating the already-exists refusal.
//
// That one is not a failure — a retry finding its own earlier work — and the
// service needs to tell it apart from every other unprocessable request, which
// GitHub reports with the same status.
func (p *Port) CreateBranch(ctx context.Context, githubInstallationID int64, owner, repo, name, sha string) (application.RemoteBranch, error) {
	ref, err := p.client.CreateBranch(ctx, githubInstallationID, owner, repo, name, sha)
	if err != nil {
		if errors.Is(err, ErrRefExists) {
			return application.RemoteBranch{}, application.ErrRemoteRefExists
		}
		return application.RemoteBranch{}, err
	}
	return application.RemoteBranch{Name: ref.Name, SHA: ref.SHA}, nil
}

// BranchRules reads the rules that would apply to a branch name, existing or
// not, and reports whether they govern writing the ref.
func (p *Port) BranchRules(ctx context.Context, githubInstallationID int64, owner, repo, branch string) (application.BranchRule, error) {
	rules, err := p.client.BranchRules(ctx, githubInstallationID, owner, repo, branch)
	if err != nil {
		return application.BranchRule{}, err
	}
	name, restricted := rules.RestrictsWriting()
	return application.BranchRule{Restricted: restricted, Rule: name}, nil
}
