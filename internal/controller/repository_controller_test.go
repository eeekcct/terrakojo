/*
Copyright 2025 eeekcct.

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
	"fmt"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	terrakojoiov1alpha1 "github.com/eeekcct/terrakojo/api/v1alpha1"
	gh "github.com/eeekcct/terrakojo/internal/github"
	ghapi "github.com/google/go-github/v79/github"
)

var _ = Describe("Repository Reconciler", func() {
	var (
		mgr       ctrl.Manager
		mgrCtx    context.Context
		mgrCancel context.CancelFunc
		mgrErrCh  chan error
		ghManager *fakeGitHubClientManager
	)

	BeforeEach(func() {
		skipNameValidation := true
		var err error
		mgr, err = ctrl.NewManager(cfg, ctrl.Options{
			Scheme:                 scheme.Scheme,
			Metrics:                server.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
			WebhookServer:          webhook.NewServer(webhook.Options{Port: 0}),
			Controller:             config.Controller{SkipNameValidation: &skipNameValidation},
		})
		Expect(err).NotTo(HaveOccurred())

		ghManager = &fakeGitHubClientManager{}
		reconciler := &RepositoryReconciler{
			Client:              mgr.GetClient(),
			Scheme:              mgr.GetScheme(),
			GitHubClientManager: ghManager,
		}
		Expect(reconciler.SetupWithManager(mgr)).To(Succeed())

		mgrCtx, mgrCancel = context.WithCancel(context.Background())
		mgrErrCh = make(chan error, 1)
		go func() {
			defer GinkgoRecover()
			mgrErrCh <- mgr.Start(mgrCtx)
		}()

		Expect(mgr.GetCache().WaitForCacheSync(mgrCtx)).To(BeTrue())

		DeferCleanup(func() {
			mgrCancel()
			err := <-mgrErrCh
			if err != nil && !errors.Is(err, context.Canceled) {
				Expect(err).NotTo(HaveOccurred())
			}
		})
	})

	When("reconciling Repository resources", func() {
		const namespace = "default"

		It("adds finalizer and required labels", func() {
			ctx := context.Background()
			repo := &terrakojoiov1alpha1.Repository{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      "repo-finalizer-labels",
					Namespace: namespace,
				},
				Spec: terrakojoiov1alpha1.RepositorySpec{
					Owner:         "test-owner",
					Name:          "test-repo",
					Type:          "github",
					DefaultBranch: "main",
					GitHubSecretRef: terrakojoiov1alpha1.GitHubSecretRef{
						Name: "github-secret",
					},
				},
			}
			Expect(k8sClient.Create(ctx, repo)).To(Succeed())

			key := types.NamespacedName{Name: repo.Name, Namespace: repo.Namespace}
			Eventually(func(g Gomega) {
				fetched := &terrakojoiov1alpha1.Repository{}
				g.Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
				g.Expect(fetched.Finalizers).To(ContainElement(repositoryFinalizer))
				g.Expect(fetched.Labels).To(HaveKeyWithValue("terrakojo.io/owner", repo.Spec.Owner))
				g.Expect(fetched.Labels).To(HaveKeyWithValue("terrakojo.io/repo-name", repo.Spec.Name))
				g.Expect(fetched.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "terrakojo-controller"))
				g.Expect(fetched.Labels).To(HaveKeyWithValue("terrakojo.io/repo-uid", string(fetched.UID)))
			}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
		})

		It("creates Branch resources for default branch commits", func() {
			ctx := context.Background()
			sha := "0123456789abcdef0123456789abcdef01234567"
			ghManager.GetClientForRepositoryFunc = func(ctx context.Context, repo *terrakojoiov1alpha1.Repository) (gh.ClientInterface, error) {
				return &fakeGitHubClient{
					GetBranchFunc: func(owner, repoName, branchName string) (*ghapi.Branch, error) {
						return &ghapi.Branch{
							Name: ghapi.Ptr(branchName),
							Commit: &ghapi.RepositoryCommit{
								SHA: ghapi.Ptr(sha),
							},
						}, nil
					},
				}, nil
			}
			repo := &terrakojoiov1alpha1.Repository{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      "repo-default-commits",
					Namespace: namespace,
				},
				Spec: terrakojoiov1alpha1.RepositorySpec{
					Owner:         "test-owner",
					Name:          "test-repo",
					Type:          "github",
					DefaultBranch: "main",
					GitHubSecretRef: terrakojoiov1alpha1.GitHubSecretRef{
						Name: "github-secret",
					},
				},
			}
			Expect(k8sClient.Create(ctx, repo)).To(Succeed())

			branchName := fmt.Sprintf("%s-%s-%s", repo.Spec.Name, hashRef("main"), shortSHA(sha))
			branchKey := types.NamespacedName{Name: branchName, Namespace: repo.Namespace}
			Eventually(func(g Gomega) {
				branch := &terrakojoiov1alpha1.Branch{}
				g.Expect(k8sClient.Get(ctx, branchKey, branch)).To(Succeed())
				g.Expect(branch.Spec.Owner).To(Equal(repo.Spec.Owner))
				g.Expect(branch.Spec.Repository).To(Equal(repo.Spec.Name))
				g.Expect(branch.Spec.Name).To(Equal("main"))
				g.Expect(branch.Spec.SHA).To(Equal(sha))
			}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
		})

		It("creates Branch resources for non-default BranchList entries", func() {
			ctx := context.Background()
			ref := "feature/test"
			sha := "abcdef0123456789abcdef0123456789abcdef01"
			ghManager.GetClientForRepositoryFunc = func(ctx context.Context, repo *terrakojoiov1alpha1.Repository) (gh.ClientInterface, error) {
				return &fakeGitHubClient{
					GetBranchFunc: func(owner, repoName, branchName string) (*ghapi.Branch, error) {
						return &ghapi.Branch{Name: ghapi.Ptr(branchName)}, nil
					},
					ListBranchesFunc: func(owner, repoName string) ([]*ghapi.Branch, error) {
						return []*ghapi.Branch{
							{Name: ghapi.Ptr("main"), Commit: &ghapi.RepositoryCommit{SHA: ghapi.Ptr("sha-main")}},
							{Name: ghapi.Ptr(ref), Commit: &ghapi.RepositoryCommit{SHA: ghapi.Ptr(sha)}},
						}, nil
					},
					ListOpenPullRequestsFunc: func(owner, repoName string) ([]*ghapi.PullRequest, error) {
						return []*ghapi.PullRequest{
							{
								Number: ghapi.Ptr(12),
								Head:   &ghapi.PullRequestBranch{Ref: ghapi.Ptr(ref)},
							},
						}, nil
					},
				}, nil
			}
			repo := &terrakojoiov1alpha1.Repository{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      "repo-branch-list",
					Namespace: namespace,
				},
				Spec: terrakojoiov1alpha1.RepositorySpec{
					Owner:         "test-owner",
					Name:          "test-repo",
					Type:          "github",
					DefaultBranch: "main",
					GitHubSecretRef: terrakojoiov1alpha1.GitHubSecretRef{
						Name: "github-secret",
					},
				},
			}
			Expect(k8sClient.Create(ctx, repo)).To(Succeed())

			branchName := fmt.Sprintf("%s-%s-%s", repo.Spec.Name, hashRef(ref), shortSHA(sha))
			branchKey := types.NamespacedName{Name: branchName, Namespace: repo.Namespace}
			Eventually(func(g Gomega) {
				branch := &terrakojoiov1alpha1.Branch{}
				g.Expect(k8sClient.Get(ctx, branchKey, branch)).To(Succeed())
				g.Expect(branch.Spec.Name).To(Equal(ref))
				g.Expect(branch.Spec.SHA).To(Equal(sha))
				g.Expect(branch.Spec.PRNumber).To(Equal(12))
			}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
		})

		It("deletes stale Branch resources not present in GitHub", func() {
			ctx := context.Background()
			ghManager.GetClientForRepositoryFunc = func(ctx context.Context, repo *terrakojoiov1alpha1.Repository) (gh.ClientInterface, error) {
				return &fakeGitHubClient{
					GetBranchFunc: func(owner, repoName, branchName string) (*ghapi.Branch, error) {
						return &ghapi.Branch{Name: ghapi.Ptr(branchName)}, nil
					},
					ListBranchesFunc: func(owner, repoName string) ([]*ghapi.Branch, error) {
						return []*ghapi.Branch{
							{Name: ghapi.Ptr("main"), Commit: &ghapi.RepositoryCommit{SHA: ghapi.Ptr("sha-main")}},
						}, nil
					},
				}, nil
			}
			repo := &terrakojoiov1alpha1.Repository{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      "repo-stale-branches",
					Namespace: namespace,
				},
				Spec: terrakojoiov1alpha1.RepositorySpec{
					Owner:         "test-owner",
					Name:          "test-repo",
					Type:          "github",
					DefaultBranch: "main",
					GitHubSecretRef: terrakojoiov1alpha1.GitHubSecretRef{
						Name: "github-secret",
					},
				},
			}
			Expect(k8sClient.Create(ctx, repo)).To(Succeed())

			// Create a stale branch owned by the repo.
			staleSHA := "1111111111111111111111111111111111111111"
			staleRef := "feature/stale"
			staleName := fmt.Sprintf("%s-%s-%s", repo.Spec.Name, hashRef(staleRef), shortSHA(staleSHA))
			stale := &terrakojoiov1alpha1.Branch{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      staleName,
					Namespace: repo.Namespace,
				},
				Spec: terrakojoiov1alpha1.BranchSpec{
					Owner:      repo.Spec.Owner,
					Repository: repo.Spec.Name,
					Name:       staleRef,
					SHA:        staleSHA,
				},
			}
			Expect(controllerutil.SetControllerReference(repo, stale, mgr.GetScheme())).To(Succeed())
			Expect(k8sClient.Create(ctx, stale)).To(Succeed())

			staleKey := types.NamespacedName{Name: staleName, Namespace: repo.Namespace}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, staleKey, &terrakojoiov1alpha1.Branch{})
				return apierrors.IsNotFound(err)
			}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(BeTrue())
		})
	})
})

var _ = Describe("Repository Reconciler error paths (fake client)", func() {
	DescribeTable("returns expected errors",
		func(setup func() (*RepositoryReconciler, ctrl.Request), wantErr string) {
			reconciler, req := setup()
			_, err := reconciler.Reconcile(context.Background(), req)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(wantErr))
		},
		Entry("GitHubClientManager not initialized", func() (*RepositoryReconciler, ctrl.Request) {
			testScheme := newSchemeForGinkgo()
			repo := newTestRepository("repo-gh-nil", types.UID("repo-gh-nil-uid"))
			prepareRepoForReconcile(repo)
			baseClient := newFakeClientWithIndex(testScheme, repo)
			reconciler := &RepositoryReconciler{
				Client: baseClient,
				Scheme: testScheme,
			}
			return reconciler, ctrl.Request{NamespacedName: types.NamespacedName{Name: repo.Name, Namespace: repo.Namespace}}
		}, "GitHubClientManager not initialized"),
		Entry("GetClientForRepository error", func() (*RepositoryReconciler, ctrl.Request) {
			testScheme := newSchemeForGinkgo()
			repo := newTestRepository("repo-gh-error", types.UID("repo-gh-error-uid"))
			prepareRepoForReconcile(repo)
			baseClient := newFakeClientWithIndex(testScheme, repo)
			manager := &fakeGitHubClientManager{
				GetClientForRepositoryFunc: func(ctx context.Context, repo *terrakojoiov1alpha1.Repository) (gh.ClientInterface, error) {
					return nil, fmt.Errorf("gh auth failed")
				},
			}
			reconciler := &RepositoryReconciler{
				Client:              baseClient,
				Scheme:              testScheme,
				GitHubClientManager: manager,
			}
			return reconciler, ctrl.Request{NamespacedName: types.NamespacedName{Name: repo.Name, Namespace: repo.Namespace}}
		}, "gh auth failed"),
		Entry("List Branch resources error", func() (*RepositoryReconciler, ctrl.Request) {
			testScheme := newSchemeForGinkgo()
			repo := newTestRepository("repo-list-error", types.UID("repo-list-error-uid"))
			prepareRepoForReconcile(repo)
			baseClient := newFakeClientWithIndex(testScheme, repo)
			reconciler := &RepositoryReconciler{
				Client:              &listErrorClient{Client: baseClient, err: fmt.Errorf("list failed")},
				Scheme:              testScheme,
				GitHubClientManager: &fakeGitHubClientManager{},
			}
			return reconciler, ctrl.Request{NamespacedName: types.NamespacedName{Name: repo.Name, Namespace: repo.Namespace}}
		}, "list failed"),
		Entry("ensureDefaultBranchCommits error", func() (*RepositoryReconciler, ctrl.Request) {
			testScheme := newSchemeForGinkgo()
			repo := newTestRepository("repo-default-sync-error", types.UID("repo-default-sync-error-uid"))
			prepareRepoForReconcile(repo)
			baseClient := newFakeClientWithIndex(testScheme, repo)
			manager := &fakeGitHubClientManager{
				GetClientForRepositoryFunc: func(ctx context.Context, repo *terrakojoiov1alpha1.Repository) (gh.ClientInterface, error) {
					return &fakeGitHubClient{
						GetBranchFunc: func(owner, repoName, branchName string) (*ghapi.Branch, error) {
							return &ghapi.Branch{
								Name: ghapi.Ptr(branchName),
								Commit: &ghapi.RepositoryCommit{
									SHA: ghapi.Ptr("0123456789abcdef0123456789abcdef01234567"),
								},
							}, nil
						},
					}, nil
				},
			}
			reconciler := &RepositoryReconciler{
				Client:              &createErrorClient{Client: baseClient, err: fmt.Errorf("create failed")},
				Scheme:              testScheme,
				GitHubClientManager: manager,
			}
			return reconciler, ctrl.Request{NamespacedName: types.NamespacedName{Name: repo.Name, Namespace: repo.Namespace}}
		}, "create failed"),
		Entry("syncBranchHeads error", func() (*RepositoryReconciler, ctrl.Request) {
			testScheme := newSchemeForGinkgo()
			repo := newTestRepository("repo-branchlist-error", types.UID("repo-branchlist-error-uid"))
			prepareRepoForReconcile(repo)
			oldSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			newSHA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			existing := newTestBranch(repo, "feature/error", oldSHA, 0)
			Expect(controllerutil.SetControllerReference(repo, existing, testScheme)).To(Succeed())
			baseClient := newFakeClientWithIndex(testScheme, repo, existing)
			manager := &fakeGitHubClientManager{
				GetClientForRepositoryFunc: func(ctx context.Context, repo *terrakojoiov1alpha1.Repository) (gh.ClientInterface, error) {
					return &fakeGitHubClient{
						GetBranchFunc: func(owner, repoName, branchName string) (*ghapi.Branch, error) {
							return &ghapi.Branch{Name: ghapi.Ptr(branchName)}, nil
						},
						ListBranchesFunc: func(owner, repoName string) ([]*ghapi.Branch, error) {
							return []*ghapi.Branch{
								{Name: ghapi.Ptr("feature/error"), Commit: &ghapi.RepositoryCommit{SHA: ghapi.Ptr(newSHA)}},
							}, nil
						},
					}, nil
				},
			}
			reconciler := &RepositoryReconciler{
				Client:              &deleteErrorClient{Client: baseClient, err: fmt.Errorf("delete failed")},
				Scheme:              testScheme,
				GitHubClientManager: manager,
			}
			return reconciler, ctrl.Request{NamespacedName: types.NamespacedName{Name: repo.Name, Namespace: repo.Namespace}}
		}, "delete failed"),
		Entry("ensureLabels update error", func() (*RepositoryReconciler, ctrl.Request) {
			testScheme := newSchemeForGinkgo()
			repo := newTestRepository("repo-label-update-error", types.UID("repo-label-update-error-uid"))
			repo.Finalizers = []string{repositoryFinalizer}
			baseClient := newFakeClientWithIndex(testScheme, repo)
			reconciler := &RepositoryReconciler{
				Client:              &updateErrorClient{Client: baseClient, err: fmt.Errorf("update failed")},
				Scheme:              testScheme,
				GitHubClientManager: &fakeGitHubClientManager{},
			}
			return reconciler, ctrl.Request{NamespacedName: types.NamespacedName{Name: repo.Name, Namespace: repo.Namespace}}
		}, "failed to update Repository labels"),
		Entry("syncBranchHeads stale delete error", func() (*RepositoryReconciler, ctrl.Request) {
			testScheme := newSchemeForGinkgo()
			repo := newTestRepository("repo-branchlist-stale-delete", types.UID("repo-branchlist-stale-delete-uid"))
			prepareRepoForReconcile(repo)
			stale := newTestBranch(repo, "feature/stale", "1111111111111111111111111111111111111111", 0)
			Expect(controllerutil.SetControllerReference(repo, stale, testScheme)).To(Succeed())
			baseClient := newFakeClientWithIndex(testScheme, repo, stale)
			manager := &fakeGitHubClientManager{
				GetClientForRepositoryFunc: func(ctx context.Context, repo *terrakojoiov1alpha1.Repository) (gh.ClientInterface, error) {
					return &fakeGitHubClient{
						GetBranchFunc: func(owner, repoName, branchName string) (*ghapi.Branch, error) {
							return &ghapi.Branch{Name: ghapi.Ptr(branchName)}, nil
						},
						ListBranchesFunc: func(owner, repoName string) ([]*ghapi.Branch, error) {
							return []*ghapi.Branch{}, nil
						},
					}, nil
				},
			}
			reconciler := &RepositoryReconciler{
				Client:              &deleteErrorClient{Client: baseClient, err: fmt.Errorf("delete failed")},
				Scheme:              testScheme,
				GitHubClientManager: manager,
			}
			return reconciler, ctrl.Request{NamespacedName: types.NamespacedName{Name: repo.Name, Namespace: repo.Namespace}}
		}, "delete failed"),
		Entry("updateBranchResource error", func() (*RepositoryReconciler, ctrl.Request) {
			testScheme := newSchemeForGinkgo()
			repo := newTestRepository("repo-branch-update-error", types.UID("repo-branch-update-error-uid"))
			prepareRepoForReconcile(repo)
			ref := "feature/update"
			sha := "3333333333333333333333333333333333333333"
			existing := newTestBranch(repo, ref, sha, 1)
			Expect(controllerutil.SetControllerReference(repo, existing, testScheme)).To(Succeed())
			baseClient := newFakeClientWithIndex(testScheme, repo, existing)
			manager := &fakeGitHubClientManager{
				GetClientForRepositoryFunc: func(ctx context.Context, repo *terrakojoiov1alpha1.Repository) (gh.ClientInterface, error) {
					return &fakeGitHubClient{
						GetBranchFunc: func(owner, repoName, branchName string) (*ghapi.Branch, error) {
							return &ghapi.Branch{Name: ghapi.Ptr(branchName)}, nil
						},
						ListBranchesFunc: func(owner, repoName string) ([]*ghapi.Branch, error) {
							return []*ghapi.Branch{
								{Name: ghapi.Ptr(ref), Commit: &ghapi.RepositoryCommit{SHA: ghapi.Ptr(sha)}},
							}, nil
						},
						ListOpenPullRequestsFunc: func(owner, repoName string) ([]*ghapi.PullRequest, error) {
							return []*ghapi.PullRequest{
								{
									Number: ghapi.Ptr(99),
									Head:   &ghapi.PullRequestBranch{Ref: ghapi.Ptr(ref)},
								},
							}, nil
						},
					}, nil
				},
			}
			reconciler := &RepositoryReconciler{
				Client:              &updateErrorClient{Client: baseClient, err: fmt.Errorf("update failed")},
				Scheme:              testScheme,
				GitHubClientManager: manager,
			}
			return reconciler, ctrl.Request{NamespacedName: types.NamespacedName{Name: repo.Name, Namespace: repo.Namespace}}
		}, "failed to update branch"),
	)
})

type deleteNoopClient struct {
	client.Client
}

func (c *deleteNoopClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	return nil
}

type listErrorClient struct {
	client.Client
	err error
}

func (c *listErrorClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return c.err
}

type deleteErrorClient struct {
	client.Client
	err error
}

func (c *deleteErrorClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if _, ok := obj.(*terrakojoiov1alpha1.Branch); ok {
		return c.err
	}
	return c.Client.Delete(ctx, obj, opts...)
}

type updateErrorClient struct {
	client.Client
	err error
}

func (c *updateErrorClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	return c.err
}

type createErrorClient struct {
	client.Client
	err error
}

func (c *createErrorClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, ok := obj.(*terrakojoiov1alpha1.Branch); ok {
		return c.err
	}
	return c.Client.Create(ctx, obj, opts...)
}

type updateCaptureClient struct {
	client.Client
	updated []client.Object
}

func (c *updateCaptureClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if obj != nil {
		if copied, ok := obj.DeepCopyObject().(client.Object); ok {
			c.updated = append(c.updated, copied)
		}
	}
	return c.Client.Update(ctx, obj, opts...)
}

func newTestScheme(t *testing.T) *runtime.Scheme {
	testScheme := runtime.NewScheme()
	require.NoError(t, terrakojoiov1alpha1.AddToScheme(testScheme))
	return testScheme
}

func newSchemeForGinkgo() *runtime.Scheme {
	testScheme := runtime.NewScheme()
	Expect(terrakojoiov1alpha1.AddToScheme(testScheme)).To(Succeed())
	return testScheme
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
				Name: "github-secret",
			},
		},
	}
}

func newTestBranch(repo *terrakojoiov1alpha1.Repository, ref, sha string, prNumber int) *terrakojoiov1alpha1.Branch {
	return &terrakojoiov1alpha1.Branch{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s-%s", repo.Spec.Name, hashRef(ref), shortSHA(sha)),
			Namespace: repo.Namespace,
		},
		Spec: terrakojoiov1alpha1.BranchSpec{
			Owner:      repo.Spec.Owner,
			Repository: repo.Spec.Name,
			Name:       ref,
			PRNumber:   prNumber,
			SHA:        sha,
		},
	}
}

func newFakeClientWithIndex(testScheme *runtime.Scheme, objs ...client.Object) client.Client {
	builder := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithIndex(&terrakojoiov1alpha1.Branch{}, "metadata.ownerReferences.uid", indexByOwnerRepositoryUID)
	if len(objs) > 0 {
		builder.WithObjects(objs...)
	}
	return builder.Build()
}

func prepareRepoForReconcile(repo *terrakojoiov1alpha1.Repository) {
	repo.Finalizers = []string{repositoryFinalizer}
	if repo.Labels == nil {
		repo.Labels = make(map[string]string)
	}
	repo.Labels["terrakojo.io/owner"] = repo.Spec.Owner
	repo.Labels["terrakojo.io/repo-name"] = repo.Spec.Name
	repo.Labels["terrakojo.io/repo-uid"] = string(repo.UID)
	repo.Labels["app.kubernetes.io/managed-by"] = "terrakojo-controller"
}

func TestEnsureBranchResourceScenarios(t *testing.T) {
	tests := []struct {
		name           string
		ref            string
		existingSHA    string
		existingPR     int
		newSHA         string
		newPR          int
		expectOldGone  bool
		expectNewNamed bool
	}{
		{
			name:        "updates PR number",
			ref:         "feature/pr",
			existingSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			existingPR:  1,
			newSHA:      "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			newPR:       99,
		},
		{
			name:           "replaces SHA",
			ref:            "feature/replace",
			existingSHA:    "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			existingPR:     0,
			newSHA:         "cccccccccccccccccccccccccccccccccccccccc",
			newPR:          7,
			expectOldGone:  true,
			expectNewNamed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			testScheme := newTestScheme(t)
			repo := newTestRepository("repo-"+strings.ReplaceAll(tt.name, " ", "-"), types.UID("repo-"+strings.ReplaceAll(tt.name, " ", "-")+"-uid"))
			branch := newTestBranch(repo, tt.ref, tt.existingSHA, tt.existingPR)
			require.NoError(t, controllerutil.SetControllerReference(repo, branch, testScheme))

			fakeClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(repo, branch).Build()
			reconciler := &RepositoryReconciler{
				Client: fakeClient,
				Scheme: testScheme,
			}

			err := reconciler.ensureBranchResource(ctx, repo, terrakojoiov1alpha1.BranchInfo{
				Ref:      tt.ref,
				SHA:      tt.newSHA,
				PRNumber: tt.newPR,
			}, []terrakojoiov1alpha1.Branch{*branch})
			require.NoError(t, err)

			if tt.expectOldGone {
				oldKey := types.NamespacedName{Name: branch.Name, Namespace: branch.Namespace}
				require.True(t, apierrors.IsNotFound(fakeClient.Get(ctx, oldKey, &terrakojoiov1alpha1.Branch{})))
			}

			if tt.expectNewNamed {
				newName := fmt.Sprintf("%s-%s-%s", repo.Spec.Name, hashRef(tt.ref), shortSHA(tt.newSHA))
				newKey := types.NamespacedName{Name: newName, Namespace: repo.Namespace}
				created := &terrakojoiov1alpha1.Branch{}
				require.NoError(t, fakeClient.Get(ctx, newKey, created))
				require.Equal(t, tt.newSHA, created.Spec.SHA)
				require.Equal(t, tt.newPR, created.Spec.PRNumber)
				return
			}

			fetched := &terrakojoiov1alpha1.Branch{}
			require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: branch.Name, Namespace: branch.Namespace}, fetched))
			require.Equal(t, tt.newPR, fetched.Spec.PRNumber)
		})
	}
}

func TestEnsureDefaultBranchCommitsCreates(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	testScheme := newTestScheme(t)
	repo := newTestRepository("repo-default", types.UID("repo-default-uid"))
	defaultRef := repo.Spec.DefaultBranch

	shaKeep := "1111111111111111111111111111111111111111"
	shaNew := "3333333333333333333333333333333333333333"

	branchKeep := newTestBranch(repo, defaultRef, shaKeep, 0)
	require.NoError(t, controllerutil.SetControllerReference(repo, branchKeep, testScheme))

	fakeClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(repo, branchKeep).Build()
	reconciler := &RepositoryReconciler{
		Client: fakeClient,
		Scheme: testScheme,
	}

	err := reconciler.ensureDefaultBranchCommits(ctx, repo, []string{shaKeep, shaNew}, []terrakojoiov1alpha1.Branch{*branchKeep})
	require.NoError(t, err)

	keepKey := types.NamespacedName{Name: branchKeep.Name, Namespace: branchKeep.Namespace}
	require.NoError(t, fakeClient.Get(ctx, keepKey, &terrakojoiov1alpha1.Branch{}))

	newName := fmt.Sprintf("%s-%s-%s", repo.Spec.Name, hashRef(defaultRef), shortSHA(shaNew))
	newKey := types.NamespacedName{Name: newName, Namespace: repo.Namespace}
	created := &terrakojoiov1alpha1.Branch{}
	require.NoError(t, fakeClient.Get(ctx, newKey, created))
	require.Equal(t, shaNew, created.Spec.SHA)
	require.Equal(t, defaultRef, created.Spec.Name)
}

func TestReconcileDeletionScenarios(t *testing.T) {
	tests := []struct {
		name                   string
		setupClient            func(t *testing.T, repo *terrakojoiov1alpha1.Repository, testScheme *runtime.Scheme) (client.Client, client.Client, *updateCaptureClient)
		expectErrContains      string
		expectRequeueAfter     time.Duration
		expectFinalizerRemoved bool
		expectFinalizerPresent bool
	}{
		{
			name: "cleanup error",
			setupClient: func(t *testing.T, repo *terrakojoiov1alpha1.Repository, testScheme *runtime.Scheme) (client.Client, client.Client, *updateCaptureClient) {
				baseClient := newFakeClientWithIndex(testScheme, repo)
				return baseClient, &listErrorClient{Client: baseClient, err: fmt.Errorf("list failed")}, nil
			},
			expectErrContains:      "list failed",
			expectFinalizerPresent: true,
		},
		{
			name: "requeue when branches remain",
			setupClient: func(t *testing.T, repo *terrakojoiov1alpha1.Repository, testScheme *runtime.Scheme) (client.Client, client.Client, *updateCaptureClient) {
				branch := newTestBranch(repo, "main", "7777777777777777777777777777777777777777", 0)
				require.NoError(t, controllerutil.SetControllerReference(repo, branch, testScheme))
				baseClient := newFakeClientWithIndex(testScheme, repo, branch)
				return baseClient, &deleteNoopClient{Client: baseClient}, nil
			},
			expectRequeueAfter:     5 * time.Second,
			expectFinalizerPresent: true,
		},
		{
			name: "removes finalizer on success",
			setupClient: func(t *testing.T, repo *terrakojoiov1alpha1.Repository, testScheme *runtime.Scheme) (client.Client, client.Client, *updateCaptureClient) {
				baseClient := newFakeClientWithIndex(testScheme, repo)
				captureClient := &updateCaptureClient{Client: baseClient}
				return baseClient, captureClient, captureClient
			},
			expectFinalizerRemoved: true,
		},
		{
			name: "finalizer update error",
			setupClient: func(t *testing.T, repo *terrakojoiov1alpha1.Repository, testScheme *runtime.Scheme) (client.Client, client.Client, *updateCaptureClient) {
				baseClient := newFakeClientWithIndex(testScheme, repo)
				return baseClient, &updateErrorClient{Client: baseClient, err: fmt.Errorf("update failed")}, nil
			},
			expectErrContains:      "update failed",
			expectFinalizerPresent: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			testScheme := newTestScheme(t)
			safeName := strings.ReplaceAll(tt.name, " ", "-")
			repo := newTestRepository("repo-delete-"+safeName, types.UID("repo-delete-"+safeName+"-uid"))
			now := metav1.NewTime(time.Now())
			repo.DeletionTimestamp = &now
			repo.Finalizers = []string{repositoryFinalizer}

			baseClient, clientToUse, captureClient := tt.setupClient(t, repo, testScheme)
			reconciler := &RepositoryReconciler{
				Client: clientToUse,
				Scheme: testScheme,
			}

			result, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: repo.Name, Namespace: repo.Namespace},
			})

			if tt.expectErrContains != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.expectErrContains)
			} else {
				require.NoError(t, err)
			}

			if tt.expectRequeueAfter != 0 {
				require.Equal(t, tt.expectRequeueAfter, result.RequeueAfter)
			}

			if tt.expectFinalizerRemoved {
				require.NotNil(t, captureClient)
				var updatedRepo *terrakojoiov1alpha1.Repository
				for _, obj := range captureClient.updated {
					if candidate, ok := obj.(*terrakojoiov1alpha1.Repository); ok {
						updatedRepo = candidate
						break
					}
				}
				require.NotNil(t, updatedRepo)
				require.NotContains(t, updatedRepo.Finalizers, repositoryFinalizer)
			}

			if tt.expectFinalizerPresent {
				fetched := &terrakojoiov1alpha1.Repository{}
				require.NoError(t, baseClient.Get(ctx, types.NamespacedName{Name: repo.Name, Namespace: repo.Namespace}, fetched))
				require.Contains(t, fetched.Finalizers, repositoryFinalizer)
			}
		})
	}
}
