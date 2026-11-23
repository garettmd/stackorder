package gh

import "context"

// GitCommit carries the tree and ordered parents of a Git commit.
type GitCommit struct {
	SHA     string      `json:"sha"`
	Tree    GitObject   `json:"tree"`
	Parents []GitObject `json:"parents"`
}

type GitObject struct {
	SHA string `json:"sha"`
}

func (c *Client) GetGitCommit(ctx context.Context, repo, sha string) (*GitCommit, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	ref, err := segment("commit SHA", sha)
	if err != nil {
		return nil, err
	}
	var commit GitCommit
	if err := c.get(ctx, "/repos/{owner}/{repo}/git/commits/{commit_sha}", rp+"/git/commits/"+ref, &commit); err != nil {
		return nil, err
	}
	return &commit, nil
}
