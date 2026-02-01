package sync

import (
	"fmt"
	"testing"

	gh "github.com/eeekcct/terrakojo/internal/github"
	ghapi "github.com/google/go-github/v79/github"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	terrakojoiov1alpha1 "github.com/eeekcct/terrakojo/api/v1alpha1"
)

type fakeGitHubClient struct {
	GetChangedFilesFunc          func(owner, repo string, prNumber int) ([]string, error)
	GetChangedFilesForCommitFunc func(owner, repo, sha string) ([]string, error)
	GetBranchFunc                func(owner, repo, branchName string) (*ghapi.Branch, error)
	ListBranchesFunc             func(owner, repo string) ([]*ghapi.Branch, error)
	ListOpenPullRequestsFunc     func(owner, repo string) ([]*ghapi.PullRequest, error)
	CompareCommitsFunc           func(owner, repo, base, head string) ([]*ghapi.RepositoryCommit, error)
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

func (f *fakeGitHubClient) CompareCommits(owner, repo, base, head string) ([]*ghapi.RepositoryCommit, error) {
	if f.CompareCommitsFunc != nil {
		return f.CompareCommitsFunc(owner, repo, base, head)
	}
	return []*ghapi.RepositoryCommit{}, nil
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

func TestCollectDefaultBranchCommitsUsesCompare(t *testing.T) {
	repo := newTestRepository("repo-compare", types.UID("repo-compare-uid"))
	repo.Status.LastDefaultBranchHeadSHA = "base"
	ghClient := &fakeGitHubClient{
		CompareCommitsFunc: func(owner, repoName, base, head string) ([]*ghapi.RepositoryCommit, error) {
			require.Equal(t, "base", base)
			require.Equal(t, "head", head)
			return []*ghapi.RepositoryCommit{
				{SHA: ghapi.String("sha-1")},
				{SHA: ghapi.String("sha-2")},
			}, nil
		},
	}

	shas, err := CollectDefaultBranchCommits(repo, ghClient, "head")
	require.NoError(t, err)
	require.Equal(t, []string{"sha-1", "sha-2"}, shas)
}

func TestCollectDefaultBranchCommitsFallsBackToHead(t *testing.T) {
	repo := newTestRepository("repo-compare-error", types.UID("repo-compare-error-uid"))
	repo.Status.LastDefaultBranchHeadSHA = "base"
	ghClient := &fakeGitHubClient{
		CompareCommitsFunc: func(owner, repoName, base, head string) ([]*ghapi.RepositoryCommit, error) {
			return nil, fmt.Errorf("compare failed")
		},
	}

	shas, err := CollectDefaultBranchCommits(repo, ghClient, "head")
	require.NoError(t, err)
	require.Equal(t, []string{"head"}, shas)
}

func TestFetchBranchHeadsFromGitHub(t *testing.T) {
	repo := newTestRepository("repo-branches", types.UID("repo-branches-uid"))

	ghClient := &fakeGitHubClient{
		ListBranchesFunc: func(owner, repoName string) ([]*ghapi.Branch, error) {
			return []*ghapi.Branch{
				{Name: ghapi.String("main"), Commit: &ghapi.RepositoryCommit{SHA: ghapi.String("sha-main")}},
				{Name: ghapi.String("feature/old"), Commit: &ghapi.RepositoryCommit{SHA: ghapi.String("sha-old")}},
				{Name: ghapi.String("feature/new"), Commit: &ghapi.RepositoryCommit{SHA: ghapi.String("sha-new")}},
			}, nil
		},
		ListOpenPullRequestsFunc: func(owner, repoName string) ([]*ghapi.PullRequest, error) {
			return []*ghapi.PullRequest{
				{
					Number: ghapi.Int(12),
					Head:   &ghapi.PullRequestBranch{Ref: ghapi.String("feature/new")},
				},
			}, nil
		},
	}

	updated, err := FetchBranchHeadsFromGitHub(repo, ghClient)
	require.NoError(t, err)
	require.Len(t, updated, 2)

	byRef := map[string]terrakojoiov1alpha1.BranchInfo{}
	for _, entry := range updated {
		byRef[entry.Ref] = entry
	}
	require.Contains(t, byRef, "feature/old")
	require.Contains(t, byRef, "feature/new")
	require.Equal(t, "sha-old", byRef["feature/old"].SHA)
	require.Equal(t, "sha-new", byRef["feature/new"].SHA)
	require.Equal(t, 12, byRef["feature/new"].PRNumber)
}

func newTestRepository(name string, uid types.UID) *terrakojoiov1alpha1.Repository {
	return &terrakojoiov1alpha1.Repository{
		ObjectMeta: v1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       uid,
		},
		Spec: terrakojoiov1alpha1.RepositorySpec{
			Owner:         "test-owner",
			Name:          name,
			Type:          "github",
			DefaultBranch: "main",
			GitHubSecretRef: terrakojoiov1alpha1.GitHubSecretRef{
				Name: "secret",
			},
		},
	}
}
