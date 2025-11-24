package github

import (
	"context"
	"fmt"
	"net/http"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/eeekcct/terrakojo/internal/config"
	"github.com/google/go-github/v79/github"
	"golang.org/x/oauth2"
)

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
	GetBranch(owner, repo, branchName string) (*github.Branch, error)
}

type Client struct {
	ctx    context.Context
	client *github.Client
}

// NewClient creates a new GitHub client using legacy config (backward compatibility)
func NewClient(ctx context.Context, config *config.Config) (*Client, error) {
	itr, err := ghinstallation.New(
		http.DefaultTransport,
		config.GitHubAppID,
		config.GitHubInstallationID,
		[]byte(config.GitHubPrivateKeyPath),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create GitHub installation transport: %w", err)
	}

	client := github.NewClient(&http.Client{Transport: itr})
	return &Client{
		ctx:    ctx,
		client: client,
	}, nil
}

// NewClientFromCredentials creates a GitHub client from specific credentials
func NewClientFromCredentials(ctx context.Context, creds *GitHubCredentials) (*Client, error) {
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

	client := github.NewClient(httpClient)
	return &Client{
		ctx:    ctx,
		client: client,
	}, nil
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

func (c *Client) GetBranch(owner, repo, branchName string) (*github.Branch, error) {
	branch, _, err := c.client.Repositories.GetBranch(c.ctx, owner, repo, branchName, 3)
	return branch, err
}
