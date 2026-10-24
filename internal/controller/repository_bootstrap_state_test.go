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
	"slices"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	terrakojoiov1alpha1 "github.com/eeekcct/terrakojo/api/v1alpha1"
	gh "github.com/eeekcct/terrakojo/internal/github"
	ghapi "github.com/google/go-github/v79/github"
)

const (
	bootstrapStateA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	bootstrapStateB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	bootstrapStateC = "cccccccccccccccccccccccccccccccccccccccc"
	bootstrapStateD = "dddddddddddddddddddddddddddddddddddddddd"
)

func TestBootstrapResetRequestWriteFailure(t *testing.T) {
	t.Parallel()
	h := newBootstrapStateHarness(t)
	h.requestReset()
	failure := errors.New("request ID write failed")
	h.client.requestFailure = failure

	require.ErrorIs(t, h.reconcile(), failure)
	persisted := h.repository()
	require.Empty(t, persisted.Annotations[repositoryBootstrapResetIDAnnotation])
	require.Empty(t, persisted.Status.LastBootstrapResetID)
	require.Equal(t, bootstrapStateA, persisted.Status.LastDefaultBranchHeadSHA)
	require.Zero(t, h.client.statusWrites)

	// No cutoff was committed, so the first successful initialization uses D.
	h.head = bootstrapStateD
	require.NoError(t, h.reconcile())
	persisted = h.repository()
	require.Equal(t, bootstrapStateD, persisted.Status.LastDefaultBranchHeadSHA)
	require.NotEmpty(t, persisted.Status.LastBootstrapResetID)
	require.Empty(t, defaultBranchSHAs(t, h.client.Client, h.repo))
}

func TestBootstrapResetStatusWriteFailure(t *testing.T) {
	t.Parallel()
	h := newBootstrapStateHarness(t)
	h.requestReset()
	failure := errors.New("cutover status write failed")
	h.client.statusFailure = failure

	require.ErrorIs(t, h.reconcile(), failure)
	persisted := h.repository()
	requestID := persisted.Annotations[repositoryBootstrapResetIDAnnotation]
	require.NotEmpty(t, requestID)
	require.Empty(t, persisted.Status.LastBootstrapResetID)
	require.Equal(t, bootstrapStateA, persisted.Status.LastDefaultBranchHeadSHA,
		"writing the failure condition must not commit the rejected cutover")
	require.True(t, meta.IsStatusConditionFalse(persisted.Status.Conditions, repositoryConditionBootstrapReady))

	// The reset ID survives restart, but its cutover is not applied until this
	// status update succeeds. Cursor and applied ID must become durable together.
	h.head = bootstrapStateD
	require.NoError(t, h.reconcile())
	persisted = h.repository()
	require.Equal(t, requestID, persisted.Status.LastBootstrapResetID)
	require.Equal(t, bootstrapStateD, persisted.Status.LastDefaultBranchHeadSHA)
	require.True(t, meta.IsStatusConditionTrue(persisted.Status.Conditions, repositoryConditionBootstrapReady))
	require.Empty(t, defaultBranchSHAs(t, h.client.Client, h.repo))
}

func TestBootstrapResetRetryKeepsAdvancedCursor(t *testing.T) {
	t.Parallel()
	h := newBootstrapStateHarness(t)
	h.requestReset()
	h.client.doneFailures = 2

	require.Error(t, h.reconcile()) // Cutover B is persisted; acknowledgment fails.
	requestID := h.repository().Status.LastBootstrapResetID
	require.NotEmpty(t, requestID)
	h.head = bootstrapStateC
	require.Error(t, h.reconcile()) // C is processed; acknowledgment fails again.
	require.Equal(t, bootstrapStateC, h.repository().Status.LastDefaultBranchHeadSHA)
	require.ElementsMatch(t, []string{bootstrapStateC}, defaultBranchSHAs(t, h.client.Client, h.repo))

	// Completed default-branch Workflows allow their Branch to be deleted.
	// A retry must not rewind to B and recreate that already-processed commit.
	completed := newTestBranch(h.repo, h.repo.Spec.DefaultBranch, bootstrapStateC, 0)
	require.NoError(t, h.client.Delete(context.Background(), completed))
	h.head = bootstrapStateD
	require.NoError(t, h.reconcile())
	require.Equal(t, [][2]string{{bootstrapStateB, bootstrapStateC}, {bootstrapStateC, bootstrapStateD}}, h.comparisons)
	require.ElementsMatch(t, []string{bootstrapStateD}, defaultBranchSHAs(t, h.client.Client, h.repo))
	persisted := h.repository()
	require.Equal(t, requestID, persisted.Status.LastBootstrapResetID)
	require.Equal(t, repositoryBootstrapResetDone, persisted.Annotations[repositoryBootstrapResetAnnotation])
	require.Empty(t, persisted.Annotations[repositoryBootstrapResetIDAnnotation])
}

func TestBootstrapResetFreshRequestAndNoOp(t *testing.T) {
	t.Parallel()
	h := newBootstrapStateHarness(t)
	h.requestReset()
	require.NoError(t, h.reconcile())
	first := h.repository()
	firstID := first.Status.LastBootstrapResetID
	require.NotEmpty(t, firstID)
	firstCondition := *meta.FindStatusCondition(first.Status.Conditions, repositoryConditionBootstrapReady)

	writes := h.client.statusWrites
	require.NoError(t, h.reconcile())
	unchanged := h.repository()
	require.Equal(t, first.ResourceVersion, unchanged.ResourceVersion)
	require.Equal(t, writes, h.client.statusWrites, "unchanged state must not cause status-watch churn")

	// Even a fresh explicit reset at the same HEAD is a distinct request.
	h.requestReset()
	require.NoError(t, h.reconcile())
	second := h.repository()
	require.NotEmpty(t, second.Status.LastBootstrapResetID)
	require.NotEqual(t, firstID, second.Status.LastBootstrapResetID)
	require.Equal(t, bootstrapStateB, second.Status.LastDefaultBranchHeadSHA)
	require.Empty(t, second.Annotations[repositoryBootstrapResetIDAnnotation])
	require.Empty(t, defaultBranchSHAs(t, h.client.Client, h.repo))
	secondCondition := meta.FindStatusCondition(second.Status.Conditions, repositoryConditionBootstrapReady)
	require.Equal(t, firstCondition.LastTransitionTime, secondCondition.LastTransitionTime)

	// A new reset at a later HEAD deliberately skips history only once.
	h.head = bootstrapStateC
	h.requestReset()
	require.NoError(t, h.reconcile())
	third := h.repository()
	require.NotEqual(t, second.Status.LastBootstrapResetID, third.Status.LastBootstrapResetID)
	require.Equal(t, bootstrapStateC, third.Status.LastDefaultBranchHeadSHA)
	require.Empty(t, defaultBranchSHAs(t, h.client.Client, h.repo))
	h.head = bootstrapStateD
	require.NoError(t, h.reconcile())
	require.ElementsMatch(t, []string{bootstrapStateD}, defaultBranchSHAs(t, h.client.Client, h.repo))
}

func TestBootstrapResetAcknowledgmentConflictAndCancellation(t *testing.T) {
	t.Parallel()
	h := newBootstrapStateHarness(t)
	h.requestReset()
	h.client.beforeDone = func() {
		// Simulate an operator update between status persistence and ack.
		latest := h.repository()
		latest.Annotations[repositoryBootstrapResetAnnotation] = "false"
		latest.Annotations["operator-note"] = "keep me"
		require.NoError(t, h.client.Client.Update(context.Background(), latest))
	}

	require.True(t, apierrors.IsConflict(h.reconcile()))
	persisted := h.repository()
	oldID := persisted.Status.LastBootstrapResetID
	require.NotEmpty(t, oldID)
	require.Equal(t, "false", persisted.Annotations[repositoryBootstrapResetAnnotation])
	require.Equal(t, "keep me", persisted.Annotations["operator-note"])
	require.Equal(t, bootstrapStateB, persisted.Status.LastDefaultBranchHeadSHA)

	h.head = bootstrapStateC
	require.NoError(t, h.reconcile())
	persisted = h.repository()
	require.Empty(t, persisted.Annotations[repositoryBootstrapResetIDAnnotation], "observed cancellation retires the old request")
	require.Equal(t, "false", persisted.Annotations[repositoryBootstrapResetAnnotation])
	require.Equal(t, "keep me", persisted.Annotations["operator-note"])
	require.ElementsMatch(t, []string{bootstrapStateC}, defaultBranchSHAs(t, h.client.Client, h.repo))

	h.head = bootstrapStateD
	h.requestReset()
	require.NoError(t, h.reconcile())
	require.NotEqual(t, oldID, h.repository().Status.LastBootstrapResetID)
	require.Equal(t, bootstrapStateD, h.repository().Status.LastDefaultBranchHeadSHA)
}

// A timeout can mean the API server committed a write but its response was lost.
func TestBootstrapResetAmbiguousWriteOutcome(t *testing.T) {
	for _, boundary := range []string{"status", "acknowledgment"} {
		t.Run(boundary, func(t *testing.T) {
			t.Parallel()
			h := newBootstrapStateHarness(t)
			h.requestReset()
			timeout := errors.New("write committed but response timed out")
			if boundary == "status" {
				h.client.statusTimeout = timeout
			} else {
				h.client.doneTimeout = timeout
			}
			require.ErrorIs(t, h.reconcile(), timeout)
			persisted := h.repository()
			requestID := persisted.Status.LastBootstrapResetID
			require.NotEmpty(t, requestID)
			require.Equal(t, bootstrapStateB, persisted.Status.LastDefaultBranchHeadSHA)
			require.True(t, meta.IsStatusConditionTrue(persisted.Status.Conditions, repositoryConditionBootstrapReady),
				"a stale failure-condition write must not overwrite committed status")

			h.head = bootstrapStateD
			require.NoError(t, h.reconcile())
			persisted = h.repository()
			require.Equal(t, requestID, persisted.Status.LastBootstrapResetID)
			require.Equal(t, repositoryBootstrapResetDone, persisted.Annotations[repositoryBootstrapResetAnnotation])
			require.Empty(t, persisted.Annotations[repositoryBootstrapResetIDAnnotation])
			require.ElementsMatch(t, []string{bootstrapStateC, bootstrapStateD}, defaultBranchSHAs(t, h.client.Client, h.repo))
		})
	}
}

func TestBootstrapFailureConditionIsStable(t *testing.T) {
	t.Parallel()
	h := newBootstrapStateHarness(t)
	initial := h.repository()
	initial.Status.LastDefaultBranchHeadSHA = ""
	require.NoError(t, h.client.Client.Status().Update(context.Background(), initial))
	h.headError = errors.New("head lookup failed")
	require.ErrorIs(t, h.reconcile(), h.headError)
	first := h.repository()
	condition := meta.FindStatusCondition(first.Status.Conditions, repositoryConditionBootstrapReady)
	require.NotNil(t, condition)
	transition := condition.LastTransitionTime
	writes := h.client.statusWrites

	require.ErrorIs(t, h.reconcile(), h.headError)
	require.Equal(t, first.ResourceVersion, h.repository().ResourceVersion)
	require.Equal(t, writes, h.client.statusWrites, "identical failure must not write status again")

	h.headError = errors.New("different head lookup failure")
	require.ErrorIs(t, h.reconcile(), h.headError)
	changed := h.repository()
	condition = meta.FindStatusCondition(changed.Status.Conditions, repositoryConditionBootstrapReady)
	require.Contains(t, condition.Message, h.headError.Error())
	require.Equal(t, transition, condition.LastTransitionTime, "message changes are not status transitions")
	require.Equal(t, writes+1, h.client.statusWrites)

	h.headError = nil
	require.NoError(t, h.reconcile())
	require.True(t, meta.IsStatusConditionTrue(h.repository().Status.Conditions, repositoryConditionBootstrapReady))
}

type bootstrapStateHarness struct {
	t           *testing.T
	repo        *terrakojoiov1alpha1.Repository
	client      *bootstrapStateClient
	manager     *fakeGitHubClientManager
	head        string
	headError   error
	comparisons [][2]string
}

func newBootstrapStateHarness(t *testing.T) *bootstrapStateHarness {
	t.Helper()
	repo := newTestRepository("repo-bootstrap-state", types.UID("repo-bootstrap-state-uid"))
	prepareRepoForReconcile(repo)
	repo.Status.LastDefaultBranchHeadSHA = bootstrapStateA
	h := &bootstrapStateHarness{
		t: t, repo: repo, head: bootstrapStateB,
		client: &bootstrapStateClient{Client: newFakeClientWithIndexAndRepositoryStatus(newTestScheme(t), repo)},
	}
	ghClient := &fakeGitHubClient{
		GetBranchFunc: func(owner, repoName, branchName string) (*ghapi.Branch, error) {
			return &ghapi.Branch{Commit: &ghapi.RepositoryCommit{SHA: ghapi.Ptr(h.head)}}, h.headError
		},
		CompareCommitsFunc: func(owner, repoName, base, head string) (*gh.CompareResult, error) {
			h.comparisons = append(h.comparisons, [2]string{base, head})
			history := []string{bootstrapStateA, bootstrapStateB, bootstrapStateC, bootstrapStateD}
			from, to := slices.Index(history, base), slices.Index(history, head)
			require.GreaterOrEqual(t, from, 0)
			require.Greater(t, to, from)
			commits := make([]*ghapi.RepositoryCommit, 0, to-from)
			for _, sha := range history[from+1 : to+1] {
				commits = append(commits, &ghapi.RepositoryCommit{SHA: ghapi.Ptr(sha)})
			}
			return &gh.CompareResult{Status: "ahead", Commits: commits, TotalCommits: len(commits)}, nil
		},
	}
	h.manager = &fakeGitHubClientManager{
		GetClientForRepositoryFunc: func(context.Context, *terrakojoiov1alpha1.Repository) (gh.ClientInterface, error) {
			return ghClient, nil
		},
	}
	return h
}

func (h *bootstrapStateHarness) repository() *terrakojoiov1alpha1.Repository {
	h.t.Helper()
	persisted := &terrakojoiov1alpha1.Repository{}
	require.NoError(h.t, h.client.Get(context.Background(), client.ObjectKeyFromObject(h.repo), persisted))
	return persisted
}

func (h *bootstrapStateHarness) requestReset() {
	h.t.Helper()
	persisted := h.repository()
	if persisted.Annotations == nil {
		persisted.Annotations = map[string]string{}
	}
	persisted.Annotations[repositoryBootstrapResetAnnotation] = repositoryBootstrapResetRequested
	require.NoError(h.t, h.client.Client.Update(context.Background(), persisted))
}

func (h *bootstrapStateHarness) reconcile() error {
	h.t.Helper()
	// Reconstruct for every attempt to rule out reliance on in-memory state.
	r := &RepositoryReconciler{Client: h.client, Scheme: h.client.Scheme(), GitHubClientManager: h.manager}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(h.repo)})
	return err
}

type bootstrapStateClient struct {
	client.Client
	requestFailure error
	statusFailure  error
	statusTimeout  error
	doneTimeout    error
	doneFailures   int
	statusWrites   int
	beforeDone     func()
}

func (c *bootstrapStateClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if repo, ok := obj.(*terrakojoiov1alpha1.Repository); ok {
		if isBootstrapResetRequested(repo) && repo.Annotations[repositoryBootstrapResetIDAnnotation] != "" && c.requestFailure != nil {
			err := c.requestFailure
			c.requestFailure = nil
			return err
		}
		if repo.Annotations[repositoryBootstrapResetAnnotation] == repositoryBootstrapResetDone {
			if c.beforeDone != nil {
				c.beforeDone()
				c.beforeDone = nil
			}
			if c.doneFailures > 0 {
				c.doneFailures--
				return errors.New("reset acknowledgment failed")
			}
		}
	}
	err := c.Client.Update(ctx, obj, opts...)
	if err == nil && obj.GetAnnotations()[repositoryBootstrapResetAnnotation] == repositoryBootstrapResetDone && c.doneTimeout != nil {
		err = c.doneTimeout
		c.doneTimeout = nil
	}
	return err
}

func (c *bootstrapStateClient) Status() client.SubResourceWriter {
	return &bootstrapStateStatusWriter{SubResourceWriter: c.Client.Status(), parent: c}
}

type bootstrapStateStatusWriter struct {
	client.SubResourceWriter
	parent *bootstrapStateClient
}

func (w *bootstrapStateStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	w.parent.statusWrites++
	if w.parent.statusFailure != nil {
		err := w.parent.statusFailure
		w.parent.statusFailure = nil
		return err
	}
	err := w.SubResourceWriter.Update(ctx, obj, opts...)
	if err == nil && w.parent.statusTimeout != nil {
		err = w.parent.statusTimeout
		w.parent.statusTimeout = nil
	}
	return err
}

// Verify the generated CRD actually retains the durable marker. Fake clients do
// not prune unknown status fields, so this needs the real envtest API server.
var _ = Describe("Repository bootstrap status schema", func() {
	It("persists the reset identity with the cursor across metadata updates", func() {
		ctx := context.Background()
		repo := newTestRepository("repo-bootstrap-status-schema", "")
		Expect(k8sClient.Create(ctx, repo)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, repo)).To(Succeed()) })

		repo.Status.LastDefaultBranchHeadSHA = bootstrapStateB
		repo.Status.LastBootstrapResetID = "durable-reset-id"
		Expect(k8sClient.Status().Update(ctx, repo)).To(Succeed())
		stored := &terrakojoiov1alpha1.Repository{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(repo), stored)).To(Succeed())
		Expect(stored.Status.LastDefaultBranchHeadSHA).To(Equal(bootstrapStateB))
		Expect(stored.Status.LastBootstrapResetID).To(Equal("durable-reset-id"))

		stored.Annotations = map[string]string{repositoryBootstrapResetAnnotation: repositoryBootstrapResetDone}
		Expect(k8sClient.Update(ctx, stored)).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(repo), stored)).To(Succeed())
		Expect(stored.Status.LastDefaultBranchHeadSHA).To(Equal(bootstrapStateB))
		Expect(stored.Status.LastBootstrapResetID).To(Equal("durable-reset-id"))
	})
})
