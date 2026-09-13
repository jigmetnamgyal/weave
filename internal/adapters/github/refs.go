package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// ErrRefExists is returned when GitHub refuses to create a ref because one of
// that name is already there.
//
// Distinguished from every other 422 because it is the only one that is not a
// failure: a retried creation finding its own earlier work is the ordinary
// case, and invariant 5 requires the side effect to be idempotent. Everything
// else GitHub rejects at 422 stays an error.
var ErrRefExists = errors.New("github: reference already exists")

// Ref is a Git reference and the object it points at.
type Ref struct {
	Name string
	SHA  string
}

// BranchState is what GitHub reports about an existing branch.
type BranchState struct {
	Name string
	SHA  string
	// Protected is GitHub's own answer, which accounts for pattern rules and
	// rulesets that a list of branch names would not.
	Protected bool
}

// escapeRepo builds the owner/name portion of a path.
//
// Path-escaped rather than interpolated: owner and name come from GitHub, but
// they reach here through our database, and a stored value is not a reason to
// stop escaping what goes into a URL path.
func escapeRepo(owner, name string) string {
	return url.PathEscape(owner) + "/" + url.PathEscape(name)
}

// escapeRef escapes a branch name for use in a path segment that may contain
// slashes.
//
// A branch name legitimately contains slashes — `weave/task-abc` is one path
// segment to GitHub and three to a URL parser. Escaping each component keeps
// the slashes while refusing anything that would change the path's shape.
func escapeRef(name string) string {
	parts := strings.Split(name, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

// Branch reads one branch.
//
// Returns ErrNotFound when the branch does not exist, which is the ordinary
// answer before creating one rather than an error condition.
func (c *Client) Branch(ctx context.Context, installationID int64, owner, repo, branch string) (BranchState, error) {
	token, err := c.InstallationToken(ctx, installationID)
	if err != nil {
		return BranchState{}, err
	}

	path := "/repos/" + escapeRepo(owner, repo) + "/branches/" + escapeRef(branch)
	body, err := c.do(ctx, http.MethodGet, path, token, nil)
	if err != nil {
		return BranchState{}, err
	}

	var response struct {
		Name      string `json:"name"`
		Protected bool   `json:"protected"`
		Commit    struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return BranchState{}, fmt.Errorf("github: decode branch: %w", err)
	}
	return BranchState{Name: response.Name, SHA: response.Commit.SHA, Protected: response.Protected}, nil
}

// CreateBranch creates refs/heads/<name> at sha.
//
// The "already exists" case is translated rather than relayed. GitHub answers
// 422 both for a duplicate ref and for a malformed one, and the caller's
// response to those differs entirely: one is its own previous attempt
// succeeding, the other is a mistake.
func (c *Client) CreateBranch(ctx context.Context, installationID int64, owner, repo, name, sha string) (Ref, error) {
	token, err := c.InstallationToken(ctx, installationID)
	if err != nil {
		return Ref{}, err
	}

	payload, err := json.Marshal(map[string]string{
		"ref": "refs/heads/" + name,
		"sha": sha,
	})
	if err != nil {
		return Ref{}, fmt.Errorf("github: encode ref: %w", err)
	}

	path := "/repos/" + escapeRepo(owner, repo) + "/git/refs"
	body, err := c.do(ctx, http.MethodPost, path, token, bytes.NewReader(payload))
	if err != nil {
		// GitHub's message is the only thing distinguishing a duplicate from
		// any other unprocessable request; there is no distinct status or
		// error code for it.
		if strings.Contains(err.Error(), "Reference already exists") {
			return Ref{}, ErrRefExists
		}
		return Ref{}, err
	}

	var created struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		return Ref{}, fmt.Errorf("github: decode created ref: %w", err)
	}
	return Ref{Name: strings.TrimPrefix(created.Ref, "refs/heads/"), SHA: created.Object.SHA}, nil
}
