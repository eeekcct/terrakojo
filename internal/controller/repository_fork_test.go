package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	terrakojoiov1alpha1 "github.com/eeekcct/terrakojo/api/v1alpha1"
	gh "github.com/eeekcct/terrakojo/internal/github"
	ghapi "github.com/google/go-github/v79/github"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	forkTestSHA       = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	forkTestLegacyUID = "legacy-fork-uid"
)

type forkFixture struct {
	ctx        context.Context
	repo       *terrakojoiov1alpha1.Repository
	kube       client.Client
	reconciler *RepositoryReconciler
	prs        []*ghapi.PullRequest
	localHeads []*ghapi.Branch
}

func newForkFixture(t *testing.T) *forkFixture {
	t.Helper()
	f := &forkFixture{ctx: context.Background(), repo: newTestRepository("forks", types.UID("forks-uid"))}
	prepareRepoForReconcile(f.repo)
	// Fork-only scenarios begin after the default-branch cutover is initialized.
	f.repo.Status.LastDefaultBranchHeadSHA = forkTestSHA
	scheme := newTestScheme(t)
	f.kube = newFakeClientWithIndexAndRepositoryStatus(scheme, f.repo)
	github := &fakeGitHubClient{
		GetBranchFunc: func(_, _, ref string) (*ghapi.Branch, error) {
			return &ghapi.Branch{Name: ghapi.Ptr(ref), Commit: &ghapi.RepositoryCommit{SHA: ghapi.Ptr(forkTestSHA)}}, nil
		},
		ListBranchesFunc:         func(_, _ string) ([]*ghapi.Branch, error) { return f.localHeads, nil },
		ListOpenPullRequestsFunc: func(_, _ string) ([]*ghapi.PullRequest, error) { return f.prs, nil },
	}
	f.reconciler = &RepositoryReconciler{Client: f.kube, Scheme: scheme, GitHubClientManager: &fakeGitHubClientManager{
		GetClientForRepositoryFunc: func(_ context.Context, _ *terrakojoiov1alpha1.Repository) (gh.ClientInterface, error) {
			return github, nil
		},
	}}
	return f
}

func (f *forkFixture) reconcile(t *testing.T) ctrl.Result {
	t.Helper()
	result, err := f.reconciler.Reconcile(f.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.repo)})
	require.NoError(t, err)
	return result
}

func (f *forkFixture) branches(t *testing.T) map[int]terrakojoiov1alpha1.Branch {
	t.Helper()
	var list terrakojoiov1alpha1.BranchList
	require.NoError(t, f.kube.List(f.ctx, &list))
	result := make(map[int]terrakojoiov1alpha1.Branch)
	for _, branch := range list.Items {
		result[branch.Spec.PRNumber] = branch
	}
	require.Len(t, result, len(list.Items), "unexpected duplicate branch per PR")
	return result
}

func (f *forkFixture) addBranch(t *testing.T, branch *terrakojoiov1alpha1.Branch) {
	t.Helper()
	require.NoError(t, controllerutil.SetControllerReference(f.repo, branch, f.reconciler.Scheme))
	require.NoError(t, f.kube.Create(f.ctx, branch))
}

func controllerForkPR(number int, ref, sha, owner string) *ghapi.PullRequest {
	return &ghapi.PullRequest{Number: ghapi.Ptr(number), Head: &ghapi.PullRequestBranch{
		Ref: ghapi.Ptr(ref), SHA: ghapi.Ptr(sha), Repo: &ghapi.Repository{FullName: ghapi.Ptr(owner + "/forks")},
	}}
}

func TestRepositoryForkLifecycleIsolation(t *testing.T) {
	for _, ref := range []string{"feature", strings.Repeat("feature/", 40) + "tail", "pull/10/head"} {
		t.Run(ref, func(t *testing.T) {
			f := newForkFixture(t)
			f.localHeads = []*ghapi.Branch{{Name: ghapi.Ptr(ref), Commit: &ghapi.RepositoryCommit{SHA: ghapi.Ptr(forkTestSHA)}}}
			local := newTestBranch(f.repo, ref, forkTestSHA, 0)
			local.UID = "local-uid"
			f.addBranch(t, local)
			f.prs = []*ghapi.PullRequest{controllerForkPR(10, ref, forkTestSHA, "alice"), controllerForkPR(11, ref, forkTestSHA, "bob")}
			f.reconcile(t)
			branches := f.branches(t)
			require.Len(t, branches, 3)
			require.Equal(t, local.Name, branches[0].Name)
			require.Equal(t, local.UID, branches[0].UID)
			require.NotEqual(t, branches[10].Name, branches[11].Name)
			for number, branch := range branches {
				require.Equal(t, ref, branch.Spec.Name)
				require.Equal(t, f.repo.Spec.Owner, branch.Spec.Owner)
				require.Equal(t, f.repo.Spec.Name, branch.Spec.Repository)
				if number == 0 {
					continue
				}
				branch.UID = types.UID(branch.Name)
				require.NoError(t, f.kube.Update(f.ctx, &branch))
				workflow := &terrakojoiov1alpha1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: branch.Name + "-workflow", Namespace: f.repo.Namespace}}
				require.NoError(t, controllerutil.SetControllerReference(&branch, workflow, f.reconciler.Scheme))
				require.NoError(t, f.kube.Create(f.ctx, workflow))
			}
			before := f.branches(t)
			f.reconcile(t)
			require.Equal(t, before, f.branches(t), "unchanged sync must preserve every branch")

			f.prs[0].Head.SHA = ghapi.Ptr(strings.Repeat("b", 40))
			f.reconcile(t)
			after := f.branches(t)
			require.Len(t, after, 3)
			require.Equal(t, before[0], after[0])
			require.Equal(t, before[11], after[11])
			require.NotEqual(t, before[10].Name, after[10].Name)
			workflow := &terrakojoiov1alpha1.Workflow{}
			key := types.NamespacedName{Name: before[11].Name + "-workflow", Namespace: f.repo.Namespace}
			require.NoError(t, f.kube.Get(f.ctx, key, workflow))
			require.Equal(t, before[11].UID, workflow.OwnerReferences[0].UID)

			f.prs = f.prs[1:]
			f.reconcile(t)
			after = f.branches(t)
			require.Len(t, after, 2)
			require.Equal(t, before[0], after[0])
			require.Equal(t, before[11], after[11])
			require.NoError(t, f.kube.Get(f.ctx, key, workflow))
			require.True(t, workflow.DeletionTimestamp.IsZero())
		})
	}
}

func TestRepositoryForkMigrationWaitsForFinalizers(t *testing.T) {
	f := newForkFixture(t)
	ref := "feature"
	f.localHeads = []*ghapi.Branch{{Name: ghapi.Ptr(ref), Commit: &ghapi.RepositoryCommit{SHA: ghapi.Ptr(forkTestSHA)}}}
	f.prs = []*ghapi.PullRequest{controllerForkPR(10, ref, forkTestSHA, "alice")}
	legacy := newTestBranch(f.repo, ref, forkTestSHA, 10)
	legacy.UID = forkTestLegacyUID
	legacy.Finalizers = []string{branchFinalizer}
	f.addBranch(t, legacy)
	require.Equal(t, time.Second, f.reconcile(t).RequeueAfter)
	pending := &terrakojoiov1alpha1.Branch{}
	require.NoError(t, f.kube.Get(f.ctx, client.ObjectKeyFromObject(legacy), pending))
	require.False(t, pending.DeletionTimestamp.IsZero())
	require.Equal(t, 10, pending.Spec.PRNumber, "must not repurpose the legacy fork as a local branch")
	require.Len(t, f.branches(t), 1)
	require.Equal(t, time.Second, f.reconcile(t).RequeueAfter)
	require.NoError(t, f.kube.Get(f.ctx, client.ObjectKeyFromObject(legacy), pending))
	pending.Finalizers = nil // Simulate completion of the existing Branch cleanup controller.
	require.NoError(t, f.kube.Update(f.ctx, pending))
	f.reconcile(t)
	branches := f.branches(t)
	require.Len(t, branches, 2)
	require.Equal(t, legacy.Name, branches[0].Name)
	require.NotEqual(t, legacy.UID, branches[0].UID)
	require.NotEqual(t, legacy.Name, branches[10].Name)
	require.Equal(t, "10", branches[10].Annotations[externalPRIdentityAnnotation])
	f.reconcile(t)
	require.Equal(t, branches, f.branches(t))
}

func TestRepositoryMissingForkSHADoesNotAdoptLegacyAsLocal(t *testing.T) {
	f := newForkFixture(t)
	f.localHeads = []*ghapi.Branch{{Name: ghapi.Ptr("feature"), Commit: &ghapi.RepositoryCommit{SHA: ghapi.Ptr(forkTestSHA)}}}
	f.prs = []*ghapi.PullRequest{controllerForkPR(10, "feature", "", "alice")}
	legacy := newTestBranch(f.repo, "feature", forkTestSHA, 10)
	legacy.UID = forkTestLegacyUID
	f.addBranch(t, legacy)
	f.reconcile(t)
	branches := f.branches(t)
	require.Len(t, branches, 1)
	require.Equal(t, 0, branches[0].Spec.PRNumber)
	require.NotEqual(t, legacy.UID, branches[0].UID)
}

func TestRepositoryForkDefaultRefStillExcluded(t *testing.T) {
	f := newForkFixture(t)
	base := newTestBranch(f.repo, "main", forkTestSHA, 0)
	base.UID = "default-commit-uid"
	f.addBranch(t, base)
	f.prs = []*ghapi.PullRequest{controllerForkPR(10, "main", strings.Repeat("b", 40), "alice")}
	f.reconcile(t)
	branches := f.branches(t)
	require.Len(t, branches, 1)
	require.Equal(t, base.UID, branches[0].UID)
	require.Equal(t, forkTestSHA, branches[0].Spec.SHA)
	require.Empty(t, branches[0].Annotations[externalPRIdentityAnnotation])
}

func TestRepositoryExternalPRRefRenameUpdatesLiteralRef(t *testing.T) {
	f := newForkFixture(t)
	f.prs = []*ghapi.PullRequest{controllerForkPR(10, "before", forkTestSHA, "alice")}
	f.reconcile(t)
	f.prs[0].Head.Ref = ghapi.Ptr("after")
	f.reconcile(t)
	require.Equal(t, "after", f.branches(t)[10].Spec.Name)
}

func TestRepositoryLocalPRClosurePreservesBranch(t *testing.T) {
	f := newForkFixture(t)
	f.localHeads = []*ghapi.Branch{{Name: ghapi.Ptr("feature"), Commit: &ghapi.RepositoryCommit{SHA: ghapi.Ptr(forkTestSHA)}}}
	local := newTestBranch(f.repo, "feature", forkTestSHA, 12)
	local.UID = "existing-local-uid"
	f.addBranch(t, local)
	f.prs = []*ghapi.PullRequest{controllerForkPR(12, "feature", forkTestSHA, f.repo.Spec.Owner)}
	f.reconcile(t)
	require.Equal(t, local.UID, f.branches(t)[12].UID)
	f.prs = nil
	f.reconcile(t)
	branches := f.branches(t)
	require.Len(t, branches, 1)
	require.Equal(t, local.UID, branches[0].UID)
	require.Equal(t, local.Name, branches[0].Name)
	require.Empty(t, branches[0].Annotations[externalPRIdentityAnnotation])
}

func TestRepositoryBootstrapResetSurvivesForkMigrationWait(t *testing.T) {
	f := newForkFixture(t)
	f.repo.Annotations = map[string]string{repositoryBootstrapResetAnnotation: repositoryBootstrapResetRequested}
	require.NoError(t, f.kube.Update(f.ctx, f.repo))
	legacy := newTestBranch(f.repo, "feature", forkTestSHA, 10)
	legacy.UID = forkTestLegacyUID
	legacy.Finalizers = []string{branchFinalizer}
	f.addBranch(t, legacy)

	headSHA := forkTestSHA
	nextSHA := strings.Repeat("b", 40)
	compareCalls := 0
	github := &fakeGitHubClient{
		GetBranchFunc: func(_, _, ref string) (*ghapi.Branch, error) {
			return &ghapi.Branch{Name: ghapi.Ptr(ref), Commit: &ghapi.RepositoryCommit{SHA: ghapi.Ptr(headSHA)}}, nil
		},
		ListBranchesFunc: func(_, _ string) ([]*ghapi.Branch, error) {
			return []*ghapi.Branch{{Name: ghapi.Ptr("feature"), Commit: &ghapi.RepositoryCommit{SHA: ghapi.Ptr(forkTestSHA)}}}, nil
		},
		ListOpenPullRequestsFunc: func(_, _ string) ([]*ghapi.PullRequest, error) {
			return []*ghapi.PullRequest{controllerForkPR(10, "feature", forkTestSHA, "alice")}, nil
		},
		CompareCommitsFunc: func(_, _, base, head string) (*gh.CompareResult, error) {
			compareCalls++
			require.Equal(t, forkTestSHA, base)
			require.Equal(t, nextSHA, head)
			return &gh.CompareResult{Commits: []*ghapi.RepositoryCommit{{SHA: ghapi.Ptr(nextSHA)}}}, nil
		},
	}
	f.reconciler.GitHubClientManager = &fakeGitHubClientManager{
		GetClientForRepositoryFunc: func(_ context.Context, _ *terrakojoiov1alpha1.Repository) (gh.ClientInterface, error) {
			return github, nil
		},
	}
	readRepo := func() *terrakojoiov1alpha1.Repository {
		result := &terrakojoiov1alpha1.Repository{}
		require.NoError(t, f.kube.Get(f.ctx, client.ObjectKeyFromObject(f.repo), result))
		return result
	}
	require.Equal(t, time.Second, f.reconcile(t).RequeueAfter)
	initialized := readRepo()
	resetID := initialized.Annotations[repositoryBootstrapResetIDAnnotation]
	require.NotEmpty(t, resetID)
	require.Equal(t, resetID, initialized.Status.LastBootstrapResetID)
	require.Equal(t, forkTestSHA, initialized.Status.LastDefaultBranchHeadSHA)
	require.Equal(t, repositoryBootstrapResetRequested, initialized.Annotations[repositoryBootstrapResetAnnotation])

	// HEAD can advance while the legacy fork's finalizer is still pending.
	// The same reset must process the new commit, not recapture HEAD as cutover.
	headSHA = nextSHA
	require.Equal(t, time.Second, f.reconcile(t).RequeueAfter)
	advanced := readRepo()
	require.Equal(t, 1, compareCalls)
	require.Equal(t, resetID, advanced.Status.LastBootstrapResetID)
	require.Equal(t, resetID, advanced.Annotations[repositoryBootstrapResetIDAnnotation])
	require.Equal(t, nextSHA, advanced.Status.LastDefaultBranchHeadSHA)
	require.Equal(t, initialized.Status.Conditions, advanced.Status.Conditions)
	processedCommit := newTestBranch(f.repo, f.repo.Spec.DefaultBranch, nextSHA, 0)
	require.NoError(t, f.kube.Get(f.ctx, client.ObjectKeyFromObject(processedCommit), processedCommit))
	require.NoError(t, f.kube.Delete(f.ctx, processedCommit)) // Simulate a completed commit's cleanup.

	pending := &terrakojoiov1alpha1.Branch{}
	require.NoError(t, f.kube.Get(f.ctx, client.ObjectKeyFromObject(legacy), pending))
	pending.Finalizers = nil
	require.NoError(t, f.kube.Update(f.ctx, pending))
	f.reconcile(t)
	finished := readRepo()
	require.Equal(t, repositoryBootstrapResetDone, finished.Annotations[repositoryBootstrapResetAnnotation])
	require.Empty(t, finished.Annotations[repositoryBootstrapResetIDAnnotation])
	require.Equal(t, resetID, finished.Status.LastBootstrapResetID)
	require.Equal(t, nextSHA, finished.Status.LastDefaultBranchHeadSHA)
	require.Equal(t, 1, compareCalls, "must not replay a processed/deleted default-branch commit")
	branches := f.branches(t)
	require.Len(t, branches, 2)
	require.Equal(t, "feature", branches[0].Spec.Name)
	require.Equal(t, "feature", branches[10].Spec.Name)
	require.NotEqual(t, branches[0].Name, branches[10].Name)
	require.Equal(t, "10", branches[10].Annotations[externalPRIdentityAnnotation])
}
