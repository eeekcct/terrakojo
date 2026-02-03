package controller

import (
	"context"
	"sync"

	"github.com/eeekcct/terrakojo/api/v1alpha1"
	gh "github.com/eeekcct/terrakojo/internal/github"
	"github.com/eeekcct/terrakojo/internal/kubernetes"
	ghapi "github.com/google/go-github/v79/github"
)

type fakeGitHubClientManager struct {
	mu sync.Mutex

	GetClientForRepositoryFunc func(ctx context.Context, repo *v1alpha1.Repository) (gh.ClientInterface, error)
	GetClientForBranchFunc     func(ctx context.Context, branch *v1alpha1.Branch) (gh.ClientInterface, error)

	RepoCalls   []*v1alpha1.Repository
	BranchCalls []*v1alpha1.Branch
}

var _ kubernetes.GitHubClientManagerInterface = (*fakeGitHubClientManager)(nil)

func (f *fakeGitHubClientManager) GetClientForRepository(ctx context.Context, repo *v1alpha1.Repository) (gh.ClientInterface, error) {
	f.mu.Lock()
	f.RepoCalls = append(f.RepoCalls, repo)
	f.mu.Unlock()

	if f.GetClientForRepositoryFunc != nil {
		return f.GetClientForRepositoryFunc(ctx, repo)
	}
	return &fakeGitHubClient{}, nil
}

func (f *fakeGitHubClientManager) GetClientForBranch(ctx context.Context, branch *v1alpha1.Branch) (gh.ClientInterface, error) {
	f.mu.Lock()
	f.BranchCalls = append(f.BranchCalls, branch)
	f.mu.Unlock()

	if f.GetClientForBranchFunc != nil {
		return f.GetClientForBranchFunc(ctx, branch)
	}
	return &fakeGitHubClient{}, nil
}

type fakeGitHubClient struct {
	GetChangedFilesFunc          func(owner, repo string, prNumber int) ([]string, error)
	GetChangedFilesForCommitFunc func(owner, repo, sha string) ([]string, error)
	GetBranchFunc                func(owner, repo, branchName string) (*ghapi.Branch, error)
	ListBranchesFunc             func(owner, repo string) ([]*ghapi.Branch, error)
	ListOpenPullRequestsFunc     func(owner, repo string) ([]*ghapi.PullRequest, error)
	CompareCommitsFunc           func(owner, repo, base, head string) (*gh.CompareResult, error)
	CreateCheckRunFunc           func(owner, repo, sha, name string) (*ghapi.CheckRun, error)
	UpdateCheckRunFunc           func(owner, repo string, checkRunID int64, name, status, conclusion string) error
}

var _ gh.ClientInterface = (*fakeGitHubClient)(nil)

func (f *fakeGitHubClient) GetChangedFiles(owner, repo string, prNumber int) ([]string, error) {
	if f.GetChangedFilesFunc != nil {
		return f.GetChangedFilesFunc(owner, repo, prNumber)
	}
	return nil, nil
}

func (f *fakeGitHubClient) GetChangedFilesForCommit(owner, repo, sha string) ([]string, error) {
	if f.GetChangedFilesForCommitFunc != nil {
		return f.GetChangedFilesForCommitFunc(owner, repo, sha)
	}
	return nil, nil
}

func (f *fakeGitHubClient) GetBranch(owner, repo, branchName string) (*ghapi.Branch, error) {
	if f.GetBranchFunc != nil {
		return f.GetBranchFunc(owner, repo, branchName)
	}
	return &ghapi.Branch{}, nil
}

func (f *fakeGitHubClient) ListBranches(owner, repo string) ([]*ghapi.Branch, error) {
	if f.ListBranchesFunc != nil {
		return f.ListBranchesFunc(owner, repo)
	}
	return []*ghapi.Branch{}, nil
}

func (f *fakeGitHubClient) ListOpenPullRequests(owner, repo string) ([]*ghapi.PullRequest, error) {
	if f.ListOpenPullRequestsFunc != nil {
		return f.ListOpenPullRequestsFunc(owner, repo)
	}
	return []*ghapi.PullRequest{}, nil
}

func (f *fakeGitHubClient) CompareCommits(owner, repo, base, head string) (*gh.CompareResult, error) {
	if f.CompareCommitsFunc != nil {
		return f.CompareCommitsFunc(owner, repo, base, head)
	}
	return &gh.CompareResult{
		Commits:      []*ghapi.RepositoryCommit{},
		TotalCommits: 0,
		Truncated:    false,
	}, nil
}

func (f *fakeGitHubClient) CreateCheckRun(owner, repo, sha, name string) (*ghapi.CheckRun, error) {
	if f.CreateCheckRunFunc != nil {
		return f.CreateCheckRunFunc(owner, repo, sha, name)
	}
	return &ghapi.CheckRun{}, nil
}

func (f *fakeGitHubClient) UpdateCheckRun(owner, repo string, checkRunID int64, name, status, conclusion string) error {
	if f.UpdateCheckRunFunc != nil {
		return f.UpdateCheckRunFunc(owner, repo, checkRunID, name, status, conclusion)
	}
	return nil
}
