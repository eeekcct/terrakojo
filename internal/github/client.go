package github

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/eeekcct/terrakojo/internal/config"
	"github.com/google/go-github/v79/github"
	"golang.org/x/oauth2"
)

// CompareResult contains the result of comparing commits with metadata
type CompareResult struct {
	Commits      []*github.RepositoryCommit
	TotalCommits int
	Truncated    bool
}

// GitHubAuthType represents the type of GitHub authentication
type GitHubAuthType string

const (
	GitHubAuthTypeToken     GitHubAuthType = "token"
	GitHubAuthTypeGitHubApp GitHubAuthType = "github-app"
)

// GitHubCredentials contains the credentials for GitHub authentication
type GitHubCredentials struct {
	Type         GitHubAuthType
	Token        string
	AppID        int64
	Installation int64
	PrivateKey   string
}

type ClientInterface interface {
	GetChangedFiles(owner, repo string, prNumber int) ([]string, error)
	GetChangedFilesForCommit(owner, repo, sha string) ([]string, error)
	GetCommit(owner, repo, sha string) (*github.RepositoryCommit, error)
	GetBranch(owner, repo, branchName string) (*github.Branch, error)
	ListBranches(owner, repo string) ([]*github.Branch, error)
	ListOpenPullRequests(owner, repo string) ([]*github.PullRequest, error)
	CompareCommits(owner, repo, base, head string) (*CompareResult, error)
	CreateCheckRun(owner, repo, sha, name string) (*github.CheckRun, error)
	UpdateCheckRun(owner, repo string, checkRunID int64, name, status, conclusion string) error
}

type Client struct {
	ctx    context.Context
	client *github.Client
}

// NewClient creates a new GitHub client using legacy config (backward compatibility)
func NewClient(ctx context.Context, cfg *config.Config) (*Client, error) {
	itr, err := ghinstallation.New(
		http.DefaultTransport,
		cfg.GitHubAppID,
		cfg.GitHubInstallationID,
		[]byte(cfg.GitHubPrivateKeyPath),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create GitHub installation transport: %w", err)
	}

	client, err := newGitHubClient(&http.Client{Transport: itr})
	if err != nil {
		return nil, err
	}
	return &Client{
		ctx:    ctx,
		client: client,
	}, nil
}

// NewClientFromCredentials creates a GitHub client from specific credentials
func NewClientFromCredentials(ctx context.Context, creds *GitHubCredentials) (ClientInterface, error) {
	var httpClient *http.Client

	switch creds.Type {
	case GitHubAuthTypeToken:
		// Use OAuth2 token authentication
		ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: creds.Token})
		httpClient = oauth2.NewClient(ctx, ts)

	case GitHubAuthTypeGitHubApp:
		// Use GitHub App authentication
		transport, err := ghinstallation.New(
			http.DefaultTransport,
			creds.AppID,
			creds.Installation,
			[]byte(creds.PrivateKey),
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create GitHub App transport: %w", err)
		}
		httpClient = &http.Client{Transport: transport}

	default:
		return nil, fmt.Errorf("unsupported authentication type: %s", creds.Type)
	}

	client, err := newGitHubClient(httpClient)
	if err != nil {
		return nil, err
	}
	return &Client{
		ctx:    ctx,
		client: client,
	}, nil
}

func newGitHubClient(httpClient *http.Client) (*github.Client, error) {
	apiURL := strings.TrimSpace(os.Getenv("GITHUB_API_URL"))
	uploadURL := strings.TrimSpace(os.Getenv("GITHUB_UPLOAD_URL"))
	if apiURL == "" && uploadURL == "" {
		return github.NewClient(httpClient), nil
	}

	// github.NewClient(...).WithEnterpriseURLs lets us target a custom GitHub API base URL
	// (e.g. GitHub Enterprise or a local mock server).
	if apiURL == "" {
		apiURL = "https://api.github.com/"
	}
	apiURL = ensureTrailingSlash(apiURL)

	if uploadURL == "" {
		uploadURL = apiURL
	}
	uploadURL = ensureTrailingSlash(uploadURL)

	client := github.NewClient(httpClient)
	return client.WithEnterpriseURLs(apiURL, uploadURL)
}

func ensureTrailingSlash(value string) string {
	if !strings.HasSuffix(value, "/") {
		return value + "/"
	}
	return value
}

func (c *Client) GetChangedFiles(owner, repo string, prNumber int) ([]string, error) {
	var allFiles []string
	opt := &github.ListOptions{PerPage: 100}
	for {
		files, resp, err := c.client.PullRequests.ListFiles(c.ctx, owner, repo, prNumber, opt)
		if err != nil {
			return nil, err
		}
		for _, file := range files {
			if file.Filename != nil {
				allFiles = append(allFiles, *file.Filename)
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return allFiles, nil
}

func (c *Client) GetChangedFilesForCommit(owner, repo, sha string) ([]string, error) {
	// Note: GetCommit does not support pagination. GitHub returns up to 3000 files in a single response.
	// Files beyond 3000 are not included, but this is a GitHub API limitation.
	commit, err := c.GetCommit(owner, repo, sha)
	if err != nil {
		return nil, fmt.Errorf("failed to get commit %s: %w", sha, err)
	}

	var files []string
	for _, file := range commit.Files {
		if file.Filename != nil {
			files = append(files, *file.Filename)
		}
	}
	return files, nil
}

func (c *Client) GetCommit(owner, repo, sha string) (*github.RepositoryCommit, error) {
	commit, _, err := c.client.Repositories.GetCommit(c.ctx, owner, repo, sha, nil)
	if err != nil {
		return nil, err
	}
	return commit, nil
}

func (c *Client) GetBranch(owner, repo, branchName string) (*github.Branch, error) {
	branch, _, err := c.client.Repositories.GetBranch(c.ctx, owner, repo, branchName, 3)
	return branch, err
}

func (c *Client) ListBranches(owner, repo string) ([]*github.Branch, error) {
	var allBranches []*github.Branch
	opts := &github.BranchListOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	}
	for {
		branches, resp, err := c.client.Repositories.ListBranches(c.ctx, owner, repo, opts)
		if err != nil {
			return nil, err
		}
		allBranches = append(allBranches, branches...)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return allBranches, nil
}

func (c *Client) ListOpenPullRequests(owner, repo string) ([]*github.PullRequest, error) {
	var allPRs []*github.PullRequest
	opts := &github.PullRequestListOptions{
		State:       "open",
		ListOptions: github.ListOptions{PerPage: 100},
	}
	for {
		prs, resp, err := c.client.PullRequests.List(c.ctx, owner, repo, opts)
		if err != nil {
			return nil, err
		}
		allPRs = append(allPRs, prs...)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return allPRs, nil
}

func (c *Client) CompareCommits(owner, repo, base, head string) (*CompareResult, error) {
	comparison, _, err := c.client.Repositories.CompareCommits(c.ctx, owner, repo, base, head, &github.ListOptions{PerPage: 250})
	if err != nil {
		return nil, err
	}

	totalCommits := 0
	if comparison.TotalCommits != nil {
		totalCommits = *comparison.TotalCommits
	}

	// Truncation is detected when we receive the maximum page size (250)
	// This handles both exactly 250 commits and >250 commits cases
	truncated := len(comparison.Commits) >= 250

	return &CompareResult{
		Commits:      comparison.Commits,
		TotalCommits: totalCommits,
		Truncated:    truncated,
	}, nil
}

func (c *Client) CreateCheckRun(owner, repo, sha, name string) (*github.CheckRun, error) {
	checkRun, _, err := c.client.Checks.CreateCheckRun(c.ctx, owner, repo, github.CreateCheckRunOptions{
		Name:    name,
		HeadSHA: sha,
		Status:  github.Ptr("queued"),
		Output: &github.CheckRunOutput{
			Title:   github.Ptr(name),
			Summary: github.Ptr("Check run created and queued."),
		},
	})
	return checkRun, err
}

// UpdateCheckRun updates a check run status and conclusion
func (c *Client) UpdateCheckRun(owner, repo string, checkRunID int64, name, status, conclusion string) error {
	updateOptions := github.UpdateCheckRunOptions{
		Name:   name,
		Status: &status,
	}

	// Only set conclusion for completed status
	if status == "completed" && conclusion != "" {
		updateOptions.Conclusion = &conclusion
	}

	_, _, err := c.client.Checks.UpdateCheckRun(c.ctx, owner, repo, checkRunID, updateOptions)
	if err != nil {
		return fmt.Errorf("failed to update check run: %w", err)
	}

	return nil
}
