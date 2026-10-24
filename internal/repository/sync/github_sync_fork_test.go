package sync

import (
	"errors"
	"strings"
	"testing"

	terrakojoiov1alpha1 "github.com/eeekcct/terrakojo/api/v1alpha1"
	ghapi "github.com/google/go-github/v79/github"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
)

func TestFetchBranchHeadsFromGitHubIsolatesForksWithSameRef(t *testing.T) {
	for _, sameSHA := range []bool{false, true} {
		name := "different SHAs"
		if sameSHA {
			name = "identical SHAs"
		}
		t.Run(name, func(t *testing.T) {
			repo := newTestRepository("base", types.UID("base-uid"))
			localSHA, firstSHA, secondSHA := "local-sha", "first-fork-sha", "second-fork-sha"
			if sameSHA {
				firstSHA, secondSHA = localSHA, localSHA
			}
			client := branchHeadsClient(
				[]*ghapi.Branch{githubBranch("feature/shared", localSHA)},
				[]*ghapi.PullRequest{
					githubPR(9, "feature/shared", secondSHA, githubRepo("second-fork", "base")),
					githubPR(4, "feature/shared", localSHA, githubRepo(repo.Spec.Owner, repo.Spec.Name)),
					githubPR(8, "feature/shared", firstSHA, githubRepo("first-fork", "base")),
				},
			)

			snapshot, err := FetchBranchHeadsFromGitHub(repo, client)
			require.NoError(t, err)
			byKey := branchHeadsByKey(t, snapshot)
			require.Len(t, byKey, 3)
			require.Equal(t, BranchHead{BranchInfo: terrakojoiov1alpha1.BranchInfo{
				Ref: "feature/shared", SHA: localSHA, PRNumber: 4,
			}}, byKey["feature/shared"])
			require.Equal(t, BranchHead{BranchInfo: terrakojoiov1alpha1.BranchInfo{
				Ref: "feature/shared", SHA: firstSHA, PRNumber: 8,
			}, ExternalPR: true}, byKey["pr:8"])
			require.Equal(t, BranchHead{BranchInfo: terrakojoiov1alpha1.BranchInfo{
				Ref: "feature/shared", SHA: secondSHA, PRNumber: 9,
			}, ExternalPR: true}, byKey["pr:9"])
			require.Equal(t, map[int]string{8: "feature/shared", 9: "feature/shared"}, snapshot.ExternalPRs)
		})
	}
}

func TestFetchBranchHeadsFromGitHubKeepsHighestLocalPR(t *testing.T) {
	for _, latestSHA := range []string{"latest-pr-sha", ""} {
		t.Run("latest SHA="+latestSHA, func(t *testing.T) {
			repo := newTestRepository("base", types.UID("base-uid"))
			source := githubRepo(repo.Spec.Owner, repo.Spec.Name)
			client := branchHeadsClient(
				[]*ghapi.Branch{githubBranch("feature/shared", "branch-sha")},
				[]*ghapi.PullRequest{
					githubPR(10, "feature/shared", "old-pr-sha", source),
					githubPR(20, "feature/shared", latestSHA, source),
					githubPR(15, "feature/shared", "middle-pr-sha", source),
				},
			)

			snapshot, err := FetchBranchHeadsFromGitHub(repo, client)
			require.NoError(t, err)
			require.Len(t, snapshot.Heads, 1)
			expectedSHA := latestSHA
			if expectedSHA == "" {
				expectedSHA = "branch-sha"
			}
			require.Equal(t, BranchHead{BranchInfo: terrakojoiov1alpha1.BranchInfo{
				Ref: "feature/shared", SHA: expectedSHA, PRNumber: 20,
			}}, snapshot.Heads[0])
			require.Empty(t, snapshot.ExternalPRs)
		})
	}
}

func TestFetchBranchHeadsFromGitHubKeepsEveryPRFromOneFork(t *testing.T) {
	repo := newTestRepository("base", types.UID("base-uid"))
	source := githubRepo("fork-owner", repo.Spec.Name)
	client := branchHeadsClient(nil, []*ghapi.PullRequest{
		githubPR(42, "feature/shared", "shared-sha", source),
		githubPR(41, "feature/shared", "shared-sha", source),
	})

	snapshot, err := FetchBranchHeadsFromGitHub(repo, client)
	require.NoError(t, err)
	byKey := branchHeadsByKey(t, snapshot)
	require.Len(t, byKey, 2)
	require.Equal(t, 41, byKey["pr:41"].PRNumber)
	require.Equal(t, 42, byKey["pr:42"].PRNumber)
	require.True(t, byKey["pr:41"].ExternalPR)
	require.True(t, byKey["pr:42"].ExternalPR)
	require.Equal(t, map[int]string{41: "feature/shared", 42: "feature/shared"}, snapshot.ExternalPRs)
}

func TestFetchBranchHeadsFromGitHubTreatsUnknownOriginsAsExternal(t *testing.T) {
	tests := []struct {
		name   string
		source *ghapi.Repository
	}{
		{name: "deleted source", source: nil},
		{name: "empty source", source: &ghapi.Repository{}},
		{name: "missing owner", source: &ghapi.Repository{Name: ghapi.Ptr("base")}},
		{name: "missing name", source: &ghapi.Repository{Owner: &ghapi.User{Login: ghapi.Ptr("test-owner")}}},
		{name: "ID without comparable base", source: &ghapi.Repository{ID: ghapi.Ptr(int64(123))}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newTestRepository("base", types.UID("base-uid"))
			client := branchHeadsClient(
				[]*ghapi.Branch{githubBranch("feature/shared", "local-sha")},
				[]*ghapi.PullRequest{githubPR(7, "feature/shared", "fork-sha", tt.source)},
			)

			snapshot, err := FetchBranchHeadsFromGitHub(repo, client)
			require.NoError(t, err)
			byKey := branchHeadsByKey(t, snapshot)
			require.Len(t, byKey, 2)
			require.Equal(t, "local-sha", byKey["feature/shared"].SHA)
			require.Zero(t, byKey["feature/shared"].PRNumber)
			require.False(t, byKey["feature/shared"].ExternalPR)
			require.Equal(t, "fork-sha", byKey["pr:7"].SHA)
			require.True(t, byKey["pr:7"].ExternalPR)
			require.Equal(t, map[int]string{7: "feature/shared"}, snapshot.ExternalPRs)
		})
	}
}

func TestFetchBranchHeadsFromGitHubDoesNotBorrowLocalSHAForFork(t *testing.T) {
	for _, sha := range []*string{nil, ghapi.Ptr("")} {
		name := "missing SHA"
		if sha != nil {
			name = "empty SHA"
		}
		t.Run(name, func(t *testing.T) {
			repo := newTestRepository("base", types.UID("base-uid"))
			pr := githubPR(7, "feature/shared", "unused", githubRepo("fork-owner", repo.Spec.Name))
			pr.Head.SHA = sha
			client := branchHeadsClient(
				[]*ghapi.Branch{githubBranch("feature/shared", "local-sha")},
				[]*ghapi.PullRequest{pr},
			)

			snapshot, err := FetchBranchHeadsFromGitHub(repo, client)
			require.NoError(t, err)
			require.Equal(t, []BranchHead{{BranchInfo: terrakojoiov1alpha1.BranchInfo{
				Ref: "feature/shared", SHA: "local-sha",
			}}}, snapshot.Heads)
			// Keep the origin evidence even when no usable head was returned, so
			// stale ref-keyed resources can still be identified during migration.
			require.Equal(t, map[int]string{7: "feature/shared"}, snapshot.ExternalPRs)
		})
	}
}

func TestFetchBranchHeadsFromGitHubClassifiesSourceRepository(t *testing.T) {
	tests := []struct {
		name     string
		source   *ghapi.Repository
		base     *ghapi.Repository
		external bool
	}{
		{name: "full name", source: &ghapi.Repository{FullName: ghapi.Ptr("test-owner/base")}},
		{name: "case-insensitive full name", source: &ghapi.Repository{FullName: ghapi.Ptr("TEST-OWNER/BASE")}},
		{name: "owner and name", source: githubRepo("test-owner", "base")},
		{name: "case-insensitive owner and name", source: githubRepo("TEST-OWNER", "BASE")},
		{name: "different owner", source: githubRepo("other-owner", "base"), external: true},
		{name: "different name", source: githubRepo("test-owner", "another-repo"), external: true},
		{name: "same IDs after rename", source: &ghapi.Repository{
			ID: ghapi.Ptr(int64(100)), FullName: ghapi.Ptr("old-owner/old-name"),
		}, base: &ghapi.Repository{ID: ghapi.Ptr(int64(100))}},
		{name: "different IDs override names", source: &ghapi.Repository{
			ID: ghapi.Ptr(int64(101)), FullName: ghapi.Ptr("test-owner/base"),
		}, base: &ghapi.Repository{ID: ghapi.Ptr(int64(100))}, external: true},
		{name: "no source ID falls back to name", source: githubRepo("test-owner", "base"),
			base: &ghapi.Repository{ID: ghapi.Ptr(int64(100))}},
		{name: "no base ID falls back to name", source: &ghapi.Repository{
			ID: ghapi.Ptr(int64(100)), FullName: ghapi.Ptr("test-owner/base"),
		}},
		{name: "zero IDs do not prove identity", source: &ghapi.Repository{
			ID: ghapi.Ptr(int64(0)), FullName: ghapi.Ptr("fork-owner/base"),
		}, base: &ghapi.Repository{ID: ghapi.Ptr(int64(0))}, external: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newTestRepository("base", types.UID("base-uid"))
			pr := githubPR(7, "feature/shared", "pr-sha", tt.source)
			if tt.base != nil {
				pr.Base = &ghapi.PullRequestBranch{Repo: tt.base}
			}

			snapshot, err := FetchBranchHeadsFromGitHub(repo, branchHeadsClient(nil, []*ghapi.PullRequest{pr}))
			require.NoError(t, err)
			require.Len(t, snapshot.Heads, 1)
			require.Equal(t, tt.external, snapshot.Heads[0].ExternalPR)
			require.Equal(t, "feature/shared", snapshot.Heads[0].Ref)
			require.Equal(t, "pr-sha", snapshot.Heads[0].SHA)
			if tt.external {
				require.Equal(t, "pr:7", snapshot.Heads[0].Key())
				require.Equal(t, map[int]string{7: "feature/shared"}, snapshot.ExternalPRs)
			} else {
				require.Equal(t, "feature/shared", snapshot.Heads[0].Key())
				require.Empty(t, snapshot.ExternalPRs)
			}
		})
	}
}

func TestFetchBranchHeadsFromGitHubSkipsInvalidRecords(t *testing.T) {
	repo := newTestRepository("base", types.UID("base-uid"))
	source := githubRepo("fork-owner", repo.Spec.Name)
	missingNumber := githubPR(1, "invalid/missing-number", "sha", source)
	missingNumber.Number = nil
	missingRef := githubPR(2, "unused", "sha", source)
	missingRef.Head.Ref = nil
	client := branchHeadsClient(
		[]*ghapi.Branch{
			nil,
			{},
			{Name: ghapi.Ptr("invalid/no-commit")},
			{Name: ghapi.Ptr("invalid/no-sha"), Commit: &ghapi.RepositoryCommit{}},
			githubBranch("", "invalid-empty-ref"),
			githubBranch("invalid/empty-sha", ""),
			githubBranch("main", "default-sha"),
			githubBranch("valid", "valid-sha"),
		},
		[]*ghapi.PullRequest{
			nil,
			{},
			{Number: ghapi.Ptr(1)},
			missingNumber,
			missingRef,
			githubPR(0, "invalid/zero-number", "sha", source),
			githubPR(-1, "invalid/negative-number", "sha", source),
			githubPR(3, "", "sha", source),
			githubPR(4, "valid-fork", "valid-fork-sha", source),
		},
	)

	snapshot, err := FetchBranchHeadsFromGitHub(repo, client)
	require.NoError(t, err)
	byKey := branchHeadsByKey(t, snapshot)
	require.Len(t, byKey, 2)
	require.Equal(t, "valid-sha", byKey["valid"].SHA)
	require.Equal(t, "valid-fork-sha", byKey["pr:4"].SHA)
	require.Equal(t, map[int]string{4: "valid-fork"}, snapshot.ExternalPRs)
}

func TestBranchHeadKeyKeepsRawRefsSeparateFromPRIdentity(t *testing.T) {
	for _, ref := range []string{"pr-7", "pull/7/head", "feature/" + strings.Repeat("long-name-", 40)} {
		t.Run(ref, func(t *testing.T) {
			local := BranchHead{BranchInfo: terrakojoiov1alpha1.BranchInfo{Ref: ref, SHA: "same-sha", PRNumber: 7}}
			external := BranchHead{BranchInfo: local.BranchInfo, ExternalPR: true}
			require.Equal(t, ref, local.Key())
			require.Equal(t, "pr:7", external.Key())
			require.NotEqual(t, local.Key(), external.Key())
			external.SHA = "updated-sha"
			require.Equal(t, "pr:7", external.Key(), "PR identity must survive head updates")
		})
	}
}

func TestFetchBranchHeadsFromGitHubReturnsListErrors(t *testing.T) {
	repo := newTestRepository("base", types.UID("base-uid"))
	listErr := errors.New("GitHub unavailable")
	t.Run("branches", func(t *testing.T) {
		client := &fakeGitHubClient{
			ListBranchesFunc: func(owner, name string) ([]*ghapi.Branch, error) { return nil, listErr },
			ListOpenPullRequestsFunc: func(owner, name string) ([]*ghapi.PullRequest, error) {
				t.Fatal("must not fetch PRs after failing to list branches")
				return nil, nil
			},
		}
		snapshot, err := FetchBranchHeadsFromGitHub(repo, client)
		require.ErrorIs(t, err, listErr)
		require.Nil(t, snapshot)
	})
	t.Run("pull requests", func(t *testing.T) {
		client := &fakeGitHubClient{
			ListOpenPullRequestsFunc: func(owner, name string) ([]*ghapi.PullRequest, error) { return nil, listErr },
		}
		snapshot, err := FetchBranchHeadsFromGitHub(repo, client)
		require.ErrorIs(t, err, listErr)
		require.Nil(t, snapshot)
	})
}

func githubRepo(owner, name string) *ghapi.Repository {
	return &ghapi.Repository{Owner: &ghapi.User{Login: ghapi.Ptr(owner)}, Name: ghapi.Ptr(name)}
}

func githubBranch(ref, sha string) *ghapi.Branch {
	return &ghapi.Branch{Name: ghapi.Ptr(ref), Commit: &ghapi.RepositoryCommit{SHA: ghapi.Ptr(sha)}}
}

func githubPR(number int, ref, sha string, source *ghapi.Repository) *ghapi.PullRequest {
	return &ghapi.PullRequest{
		Number: ghapi.Ptr(number),
		Head:   &ghapi.PullRequestBranch{Ref: ghapi.Ptr(ref), SHA: ghapi.Ptr(sha), Repo: source},
	}
}

func branchHeadsClient(branches []*ghapi.Branch, prs []*ghapi.PullRequest) *fakeGitHubClient {
	return &fakeGitHubClient{
		ListBranchesFunc:         func(owner, name string) ([]*ghapi.Branch, error) { return branches, nil },
		ListOpenPullRequestsFunc: func(owner, name string) ([]*ghapi.PullRequest, error) { return prs, nil },
	}
}

func branchHeadsByKey(t *testing.T, snapshot *BranchHeads) map[string]BranchHead {
	t.Helper()
	byKey := make(map[string]BranchHead, len(snapshot.Heads))
	for _, head := range snapshot.Heads {
		require.NotContains(t, byKey, head.Key(), "branch head identities must be unique")
		byKey[head.Key()] = head
	}
	return byKey
}
