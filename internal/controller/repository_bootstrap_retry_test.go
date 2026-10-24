/*
Copyright 2026 eeekcct.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	terrakojoiov1alpha1 "github.com/eeekcct/terrakojo/api/v1alpha1"
	gh "github.com/eeekcct/terrakojo/internal/github"
	ghapi "github.com/google/go-github/v79/github"
)

// A successful cursor write is the cutover, even if a later operation fails.
// Retrying must not move that cutover past commits that still need Branches.
func TestReconcileBootstrapRetry(t *testing.T) {
	tests := []struct {
		name      string
		reset     bool
		failureAt string
	}{
		{name: "initial_branch_list_failure", failureAt: "branches"},
		{name: "initial_pull_request_list_failure", failureAt: "pull_requests"},
		{name: "reset_branch_list_failure", reset: true, failureAt: "branches"},
		{name: "reset_pull_request_list_failure", reset: true, failureAt: "pull_requests"},
		{name: "reset_done_annotation_write_failure", reset: true, failureAt: "done"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			testScheme := newTestScheme(t)
			repo := newTestRepository("repo-bootstrap-retry", types.UID("repo-bootstrap-retry-uid"))
			prepareRepoForReconcile(repo)
			if tt.reset {
				repo.Status.LastDefaultBranchHeadSHA = testSHAAllAs
				repo.Annotations = map[string]string{
					repositoryBootstrapResetAnnotation: repositoryBootstrapResetRequested,
				}
			}

			const cutoffSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			const intermediateSHA = "cccccccccccccccccccccccccccccccccccccccc"
			const laterSHA = "dddddddddddddddddddddddddddddddddddddddd"
			headSHA := cutoffSHA
			failure := errors.New("injected failure after cutover was persisted")
			failSync := true
			var comparisons [][2]string
			ghClient := &fakeGitHubClient{
				GetBranchFunc: func(owner, repoName, branchName string) (*ghapi.Branch, error) {
					return &ghapi.Branch{
						Name:   ghapi.Ptr(branchName),
						Commit: &ghapi.RepositoryCommit{SHA: ghapi.Ptr(headSHA)},
					}, nil
				},
				ListBranchesFunc: func(owner, repoName string) ([]*ghapi.Branch, error) {
					if failSync && tt.failureAt == "branches" {
						return nil, failure
					}
					return []*ghapi.Branch{
						{Name: ghapi.Ptr(repo.Spec.DefaultBranch), Commit: &ghapi.RepositoryCommit{SHA: ghapi.Ptr(headSHA)}},
					}, nil
				},
				ListOpenPullRequestsFunc: func(owner, repoName string) ([]*ghapi.PullRequest, error) {
					if failSync && tt.failureAt == "pull_requests" {
						return nil, failure
					}
					return nil, nil
				},
				CompareCommitsFunc: func(owner, repoName, base, head string) (*gh.CompareResult, error) {
					comparisons = append(comparisons, [2]string{base, head})
					// Do not let a wrong comparison base produce plausible results.
					require.Equal(t, cutoffSHA, base, "retry must resume at the persisted cutover")
					require.Equal(t, laterSHA, head)
					return &gh.CompareResult{
						Status: "ahead",
						Commits: []*ghapi.RepositoryCommit{
							{SHA: ghapi.Ptr(intermediateSHA)},
							{SHA: ghapi.Ptr(laterSHA)},
						},
						TotalCommits: 2,
					}, nil
				},
			}
			ghManager := &fakeGitHubClientManager{
				GetClientForRepositoryFunc: func(context.Context, *terrakojoiov1alpha1.Repository) (gh.ClientInterface, error) {
					return ghClient, nil
				},
			}
			baseClient := newFakeClientWithIndexAndRepositoryStatus(testScheme, repo)
			doneClient := &failBootstrapDoneOnceClient{Client: baseClient}
			if tt.failureAt == "done" {
				doneClient.failure = failure
			}
			reconciler := &RepositoryReconciler{
				Client: doneClient, Scheme: testScheme, GitHubClientManager: ghManager,
			}
			req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(repo)}

			_, err := reconciler.Reconcile(ctx, req)
			require.ErrorIs(t, err, failure)
			persisted := &terrakojoiov1alpha1.Repository{}
			require.NoError(t, baseClient.Get(ctx, req.NamespacedName, persisted))
			require.Equal(t, cutoffSHA, persisted.Status.LastDefaultBranchHeadSHA,
				"the failure must occur after the cutover has been durably recorded")
			require.Empty(t, defaultBranchSHAs(t, baseClient, repo), "bootstrap must not replay historical commits")
			require.True(t, meta.IsStatusConditionTrue(persisted.Status.Conditions, repositoryConditionBootstrapReady),
				"downstream failures must not invalidate the persisted cutover")
			if tt.failureAt == "done" {
				require.Equal(t, 1, doneClient.failures)
				require.Equal(t, repositoryBootstrapResetRequested, persisted.Annotations[repositoryBootstrapResetAnnotation],
					"the rejected metadata write must not persist its in-memory done value")
			}

			// Simulate new commits and a controller restart. Only persisted state may
			// determine whether this is a retry of the same bootstrap request.
			headSHA = laterSHA
			failSync = false
			reconciler = &RepositoryReconciler{
				Client: doneClient, Scheme: testScheme, GitHubClientManager: ghManager,
			}
			for attempt := 0; attempt < 2; attempt++ {
				result, err := reconciler.Reconcile(ctx, req)
				require.NoError(t, err)
				require.Equal(t, repositorySyncInterval, result.RequeueAfter)
			}

			require.NoError(t, baseClient.Get(ctx, req.NamespacedName, persisted))
			assert.Equal(t, laterSHA, persisted.Status.LastDefaultBranchHeadSHA)
			assert.Equal(t, [][2]string{{cutoffSHA, laterSHA}}, comparisons,
				"retry must compare from the original cutover exactly once, not recapture HEAD")
			assert.ElementsMatch(t, []string{intermediateSHA, laterSHA}, defaultBranchSHAs(t, baseClient, repo),
				"every post-cutover commit needs one Branch for Workflow creation, with no historical replay or duplicates")
			condition := meta.FindStatusCondition(persisted.Status.Conditions, repositoryConditionBootstrapReady)
			require.NotNil(t, condition)
			assert.Equal(t, metav1.ConditionTrue, condition.Status,
				"successful retry must not leave BootstrapReady stuck False")
			if tt.reset {
				assert.Equal(t, repositoryBootstrapResetDone, persisted.Annotations[repositoryBootstrapResetAnnotation])
			}
		})
	}
}

// Fail only the reset acknowledgment, never the status write or setup updates.
// The next attempt reaches the underlying fake API with normal status-subresource
// behavior, so a failed write cannot accidentally persist metadata or status.
type failBootstrapDoneOnceClient struct {
	client.Client
	failure  error
	failures int
}

func (c *failBootstrapDoneOnceClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if repo, ok := obj.(*terrakojoiov1alpha1.Repository); ok &&
		repo.Annotations[repositoryBootstrapResetAnnotation] == repositoryBootstrapResetDone && c.failure != nil {
		err := c.failure
		c.failure = nil
		c.failures++
		return err
	}
	return c.Client.Update(ctx, obj, opts...)
}

func defaultBranchSHAs(t *testing.T, kubeClient client.Client, repo *terrakojoiov1alpha1.Repository) []string {
	t.Helper()
	branches := &terrakojoiov1alpha1.BranchList{}
	require.NoError(t, kubeClient.List(context.Background(), branches, client.InNamespace(repo.Namespace),
		client.MatchingFields{"metadata.ownerReferences.uid": string(repo.UID)}))
	var shas []string
	for _, branch := range branches.Items {
		if branch.Spec.Name == repo.Spec.DefaultBranch {
			shas = append(shas, branch.Spec.SHA)
		}
	}
	return shas
}
