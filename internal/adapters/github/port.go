package github

import (
	"context"

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
