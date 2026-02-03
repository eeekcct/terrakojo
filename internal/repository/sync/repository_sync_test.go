package sync

import (
	"fmt"
	"net/http"
	"testing"

	ghapi "github.com/google/go-github/v79/github"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	terrakojoiov1alpha1 "github.com/eeekcct/terrakojo/api/v1alpha1"
	gh "github.com/eeekcct/terrakojo/internal/github"
)

const testBaseSHA = "base"

type fakeGitHubClient struct {
	GetChangedFilesFunc          func(owner, repo string, prNumber int) ([]string, error)
	GetChangedFilesForCommitFunc func(owner, repo, sha string) ([]string, error)
	GetCommitFunc                func(owner, repo, sha string) (*ghapi.RepositoryCommit, error)
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

func (f *fakeGitHubClient) GetCommit(owner, repo, sha string) (*ghapi.RepositoryCommit, error) {
	if f.GetCommitFunc != nil {
		return f.GetCommitFunc(owner, repo, sha)
	}
	return &ghapi.RepositoryCommit{}, nil
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

func TestCollectDefaultBranchCommitsUsesCompare(t *testing.T) {
	repo := newTestRepository("repo-compare", types.UID("repo-compare-uid"))
	repo.Status.LastDefaultBranchHeadSHA = testBaseSHA
	ghClient := &fakeGitHubClient{
		CompareCommitsFunc: func(owner, repoName, base, head string) (*gh.CompareResult, error) {
			require.Equal(t, testBaseSHA, base)
			require.Equal(t, "head", head)
			return &gh.CompareResult{
				Commits: []*ghapi.RepositoryCommit{
					{SHA: ghapi.Ptr("sha-1")},
					{SHA: ghapi.Ptr("sha-2")},
				},
				TotalCommits: 2,
				Truncated:    false,
			}, nil
		},
	}

	shas, err := CollectDefaultBranchCommits(repo, ghClient, "head")
	require.NoError(t, err)
	require.Equal(t, []string{"sha-1", "sha-2"}, shas)
}

func TestCollectDefaultBranchCommitsFallsBackOnCompareFailure(t *testing.T) {
	repo := newTestRepository("repo-compare-error", types.UID("repo-compare-error-uid"))
	repo.Status.LastDefaultBranchHeadSHA = testBaseSHA
	ghClient := &fakeGitHubClient{
		CompareCommitsFunc: func(owner, repoName, base, head string) (*gh.CompareResult, error) {
			return nil, fmt.Errorf("compare failed")
		},
		GetCommitFunc: func(owner, repoName, sha string) (*ghapi.RepositoryCommit, error) {
			return nil, &ghapi.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotFound}}
		},
	}

	shas, err := CollectDefaultBranchCommits(repo, ghClient, "head")
	require.NoError(t, err)
	require.Equal(t, []string{"head"}, shas)
}

func TestCollectDefaultBranchCommitsReturnsErrorOnCompareFailureWithExistingBase(t *testing.T) {
	repo := newTestRepository("repo-compare-error-base-exists", types.UID("repo-compare-error-base-exists-uid"))
	repo.Status.LastDefaultBranchHeadSHA = testBaseSHA
	ghClient := &fakeGitHubClient{
		CompareCommitsFunc: func(owner, repoName, base, head string) (*gh.CompareResult, error) {
			return nil, fmt.Errorf("compare failed")
		},
		GetCommitFunc: func(owner, repoName, sha string) (*ghapi.RepositoryCommit, error) {
			return &ghapi.RepositoryCommit{}, nil
		},
	}

	shas, err := CollectDefaultBranchCommits(repo, ghClient, "head")
	require.Error(t, err)
	require.Nil(t, shas)
	require.ErrorContains(t, err, "compare failed")
}

func TestCollectDefaultBranchCommitsHandlesLargeCommitRange(t *testing.T) {
	repo := newTestRepository("repo-large", types.UID("repo-large-uid"))
	repo.Status.LastDefaultBranchHeadSHA = testBaseSHA

	// Simulate >250 commits scenario where CompareCommits returns first 250
	ghClient := &fakeGitHubClient{
		CompareCommitsFunc: func(owner, repoName, base, head string) (*gh.CompareResult, error) {
			require.Equal(t, testBaseSHA, base)
			require.Equal(t, "head", head)

			// GitHub API returns first 250 commits when total is 300
			commits := make([]*ghapi.RepositoryCommit, 250)
			for i := 0; i < 250; i++ {
				commits[i] = &ghapi.RepositoryCommit{
					SHA: ghapi.Ptr(fmt.Sprintf("sha-%d", i)),
				}
			}

			return &gh.CompareResult{
				Commits:      commits,
				TotalCommits: 300,
				Truncated:    true,
			}, nil
		},
	}

	shas, err := CollectDefaultBranchCommits(repo, ghClient, "head")
	require.NoError(t, err)
	require.Len(t, shas, 250, "should return first 250 commits")
	require.Equal(t, "sha-0", shas[0])
	require.Equal(t, "sha-249", shas[249])

	// In practice, controller would update LastDefaultBranchHeadSHA to "sha-249"
	// and next reconcile would fetch remaining 50 commits
}

func TestCollectDefaultBranchCommitsIncrementalProcessing(t *testing.T) {
	repo := newTestRepository("repo-incremental", types.UID("repo-incremental-uid"))

	// First reconcile: process first 250 commits
	repo.Status.LastDefaultBranchHeadSHA = testBaseSHA
	ghClient := &fakeGitHubClient{
		CompareCommitsFunc: func(owner, repoName, base, head string) (*gh.CompareResult, error) {
			if base == testBaseSHA && head == "head" {
				// First batch: 250 commits
				commits := make([]*ghapi.RepositoryCommit, 250)
				for i := 0; i < 250; i++ {
					commits[i] = &ghapi.RepositoryCommit{
						SHA: ghapi.Ptr(fmt.Sprintf("sha-%d", i)),
					}
				}
				return &gh.CompareResult{
					Commits:      commits,
					TotalCommits: 300,
					Truncated:    true,
				}, nil
			} else if base == "sha-249" && head == "head" {
				// Second batch: remaining 50 commits
				commits := make([]*ghapi.RepositoryCommit, 50)
				for i := 0; i < 50; i++ {
					commits[i] = &ghapi.RepositoryCommit{
						SHA: ghapi.Ptr(fmt.Sprintf("sha-%d", 250+i)),
					}
				}
				return &gh.CompareResult{
					Commits:      commits,
					TotalCommits: 50,
					Truncated:    false,
				}, nil
			}
			return nil, fmt.Errorf("unexpected base/head: %s/%s", base, head)
		},
	}

	// First reconcile
	shas, err := CollectDefaultBranchCommits(repo, ghClient, "head")
	require.NoError(t, err)
	require.Len(t, shas, 250)
	require.Equal(t, "sha-249", shas[249])

	// Controller updates LastDefaultBranchHeadSHA to last processed commit
	repo.Status.LastDefaultBranchHeadSHA = shas[len(shas)-1]

	// Second reconcile: fetch remaining commits
	shas, err = CollectDefaultBranchCommits(repo, ghClient, "head")
	require.NoError(t, err)
	require.Len(t, shas, 50)
	require.Equal(t, "sha-250", shas[0])
	require.Equal(t, "sha-299", shas[49])
}

func TestFetchBranchHeadsFromGitHub(t *testing.T) {
	repo := newTestRepository("repo-branches", types.UID("repo-branches-uid"))

	ghClient := &fakeGitHubClient{
		ListBranchesFunc: func(owner, repoName string) ([]*ghapi.Branch, error) {
			return []*ghapi.Branch{
				{Name: ghapi.Ptr("main"), Commit: &ghapi.RepositoryCommit{SHA: ghapi.Ptr("sha-main")}},
				{Name: ghapi.Ptr("feature/old"), Commit: &ghapi.RepositoryCommit{SHA: ghapi.Ptr("sha-old")}},
				{Name: ghapi.Ptr("feature/new"), Commit: &ghapi.RepositoryCommit{SHA: ghapi.Ptr("sha-new")}},
			}, nil
		},
		ListOpenPullRequestsFunc: func(owner, repoName string) ([]*ghapi.PullRequest, error) {
			return []*ghapi.PullRequest{
				{
					Number: ghapi.Ptr(12),
					Head:   &ghapi.PullRequestBranch{Ref: ghapi.Ptr("feature/new"), SHA: ghapi.Ptr("sha-new")},
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
		ObjectMeta: metav1.ObjectMeta{
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
