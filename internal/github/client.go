package github

import (
	"context"
	"net/http"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/eeekcct/terrakojo/internal/config"
	"github.com/google/go-github/v79/github"
)

type ClientInterface interface {
	GetChangedFiles(owner, repo string, prNumber int) ([]string, error)
}

type Client struct {
	ctx    context.Context
	client *github.Client
}

func NewClient(ctx context.Context, config *config.Config) (*Client, error) {
	itr, err := ghinstallation.New(http.DefaultTransport, config.GitHubAppID, config.GitHubInstallationID, []byte(config.GitHubPrivateKeyPath))
	if err != nil {
		return nil, err
	}
	client := github.NewClient(&http.Client{Transport: itr})
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
