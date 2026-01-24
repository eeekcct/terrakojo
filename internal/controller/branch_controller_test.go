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
	"sync/atomic"
	"time"

	terrakojoiov1alpha1 "github.com/eeekcct/terrakojo/api/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
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

	gh "github.com/eeekcct/terrakojo/internal/github"
)

var nameCounter uint64

func uniqueName(base string) string {
	return fmt.Sprintf("%s-%d-%d", base, GinkgoParallelProcess(), atomic.AddUint64(&nameCounter, 1))
}

func createTestNamespace(ctx context.Context) string {
	ns := &corev1.Namespace{
		ObjectMeta: ctrl.ObjectMeta{
			Name: uniqueName("ns"),
		},
	}
	Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	return ns.Name
}

func newBranchTestScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	Expect(terrakojoiov1alpha1.AddToScheme(scheme)).To(Succeed())
	return scheme
}

func newBranchFakeClient(scheme *runtime.Scheme, objs ...client.Object) client.Client {
	builder := fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(&terrakojoiov1alpha1.Workflow{}, "metadata.ownerReferences.uid", indexByOwnerBranchUID)
	if len(objs) > 0 {
		builder.WithObjects(objs...)
	}
	return builder.Build()
}

func newErrorTestBranch() *terrakojoiov1alpha1.Branch {
	return &terrakojoiov1alpha1.Branch{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "branch-error-test",
			Namespace:  "default",
			Finalizers: []string{branchFinalizer},
		},
		Spec: terrakojoiov1alpha1.BranchSpec{
			Repository: "repo",
			Owner:      "owner",
			Name:       "feature/error",
			SHA:        "0123456789abcdef0123456789abcdef01234567",
		},
	}
}

type listWorkflowTemplateErrorClient struct {
	client.Client
	err error
}

func (c *listWorkflowTemplateErrorClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	switch list.(type) {
	case *terrakojoiov1alpha1.WorkflowTemplateList:
		return c.err
	default:
		return c.Client.List(ctx, list, opts...)
	}
}

type deleteWorkflowErrorClient struct {
	client.Client
	err error
}

func (c *deleteWorkflowErrorClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if _, ok := obj.(*terrakojoiov1alpha1.Workflow); ok {
		return c.err
	}
	return c.Client.Delete(ctx, obj, opts...)
}

var _ = Describe("Branch Controller", func() {
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
			Scheme: scheme.Scheme,
			Metrics: server.Options{
				BindAddress: "0",
			},
			HealthProbeBindAddress: "0",
			WebhookServer:          webhook.NewServer(webhook.Options{Port: 0}),
			Controller:             config.Controller{SkipNameValidation: &skipNameValidation},
		})
		Expect(err).NotTo(HaveOccurred())

		ghManager = &fakeGitHubClientManager{}
		reconciler := &BranchReconciler{
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

	When("reconciling Branch resources", func() {
		It("add finalizer on creation", func() {
			ctx := context.Background()
			namespace := createTestNamespace(ctx)
			branchName := uniqueName("branch-finalizer")
			owner := uniqueName("owner")
			repoName := uniqueName("repo")
			branch := &terrakojoiov1alpha1.Branch{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      branchName,
					Namespace: namespace,
					Annotations: map[string]string{
						"terrakojo.io/last-sha": "0123456789abcdef0123456789abcdef01234567",
					},
				},
				Spec: terrakojoiov1alpha1.BranchSpec{
					Repository: repoName,
					Owner:      owner,
					Name:       "feature/finalizer",
					SHA:        "0123456789abcdef0123456789abcdef01234567",
				},
			}
			Expect(k8sClient.Create(ctx, branch)).To(Succeed())

			key := client.ObjectKeyFromObject(branch)
			Eventually(func(g Gomega) {
				fetched := &terrakojoiov1alpha1.Branch{}
				g.Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
				g.Expect(fetched.Finalizers).To(ContainElement(branchFinalizer))
			}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

		})

		It("requeues on deletion when workflows remain", func() {
			ctx := context.Background()
			namespace := createTestNamespace(ctx)
			branchName := uniqueName("branch-delete-requeue")
			workflowName := uniqueName("workflow-remains")
			owner := uniqueName("owner")
			repoName := uniqueName("repo")
			branch := &terrakojoiov1alpha1.Branch{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      branchName,
					Namespace: namespace,
					Finalizers: []string{
						branchFinalizer,
					},
					Annotations: map[string]string{
						"terrakojo.io/last-sha": "0123456789abcdef0123456789abcdef01234567",
					},
				},
				Spec: terrakojoiov1alpha1.BranchSpec{
					Repository: repoName,
					Owner:      owner,
					Name:       "feature/requeue",
					SHA:        "0123456789abcdef0123456789abcdef01234567",
				},
			}
			Expect(k8sClient.Create(ctx, branch)).To(Succeed())

			createdBranch := &terrakojoiov1alpha1.Branch{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(branch), createdBranch)).To(Succeed())

			workflow := &terrakojoiov1alpha1.Workflow{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      workflowName,
					Namespace: namespace,
					Finalizers: []string{
						workflowFinalizer,
					},
				},
				Spec: terrakojoiov1alpha1.WorkflowSpec{
					Owner:      createdBranch.Spec.Owner,
					Repository: createdBranch.Spec.Repository,
					Branch:     createdBranch.Name,
					SHA:        createdBranch.Spec.SHA,
					Template:   "tf-workflow-template",
					Path:       "infrastructure/app",
				},
			}
			Expect(controllerutil.SetControllerReference(createdBranch, workflow, mgr.GetScheme())).To(Succeed())
			Expect(k8sClient.Create(ctx, workflow)).To(Succeed())

			Expect(k8sClient.Delete(ctx, createdBranch)).To(Succeed())

			Eventually(func(g Gomega) {
				fetched := &terrakojoiov1alpha1.Branch{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(createdBranch), fetched)).To(Succeed())
				g.Expect(fetched.DeletionTimestamp.IsZero()).To(BeFalse())
				g.Expect(fetched.Finalizers).To(ContainElement(branchFinalizer))
			}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
		})

		It("removes finalizer when deleting and no workflows remain", func() {
			ctx := context.Background()
			namespace := createTestNamespace(ctx)
			branchName := uniqueName("branch-delete-clean")
			owner := uniqueName("owner")
			repoName := uniqueName("repo")
			branch := &terrakojoiov1alpha1.Branch{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      branchName,
					Namespace: namespace,
					Finalizers: []string{
						branchFinalizer,
					},
					Annotations: map[string]string{
						"terrakojo.io/last-sha": "abcdef0123456789abcdef0123456789abcdef01",
					},
				},
				Spec: terrakojoiov1alpha1.BranchSpec{
					Repository: repoName,
					Owner:      owner,
					Name:       "feature/cleanup",
					SHA:        "abcdef0123456789abcdef0123456789abcdef01",
				},
			}
			Expect(k8sClient.Create(ctx, branch)).To(Succeed())
			Expect(k8sClient.Delete(ctx, branch)).To(Succeed())

			Eventually(func() bool {
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(branch), &terrakojoiov1alpha1.Branch{})
				return apierrors.IsNotFound(err)
			}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(BeTrue())
		})

		It("deletes branch when all workflows completed", func() {
			ctx := context.Background()
			namespace := createTestNamespace(ctx)
			owner := uniqueName("owner")
			repoName := uniqueName("repo")
			repoResourceName := uniqueName("repo-cleanup")
			branchName := uniqueName("branch-completed")
			workflowName := uniqueName("workflow-completed")
			branchRef := "feature/completed"
			repo := &terrakojoiov1alpha1.Repository{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      repoResourceName,
					Namespace: namespace,
					Labels: map[string]string{
						"terrakojo.io/owner":     owner,
						"terrakojo.io/repo-name": repoName,
					},
				},
				Spec: terrakojoiov1alpha1.RepositorySpec{
					Owner:         owner,
					Name:          repoName,
					Type:          "github",
					DefaultBranch: "main",
					GitHubSecretRef: terrakojoiov1alpha1.GitHubSecretRef{
						Name: "dummy",
					},
				},
			}
			Expect(k8sClient.Create(ctx, repo)).To(Succeed())

			createdRepo := &terrakojoiov1alpha1.Repository{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(repo), createdRepo)).To(Succeed())
			createdRepo.Status.BranchList = []terrakojoiov1alpha1.BranchInfo{
				{
					Ref: branchRef,
					SHA: "0123456789abcdef0123456789abcdef01234567",
				},
			}
			Expect(k8sClient.Status().Update(ctx, createdRepo)).To(Succeed())

			branch := &terrakojoiov1alpha1.Branch{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      branchName,
					Namespace: namespace,
					Finalizers: []string{
						branchFinalizer,
					},
					Annotations: map[string]string{
						"terrakojo.io/last-sha": "0123456789abcdef0123456789abcdef01234567",
					},
				},
				Spec: terrakojoiov1alpha1.BranchSpec{
					Repository: repoName,
					Owner:      owner,
					Name:       branchRef,
					SHA:        "0123456789abcdef0123456789abcdef01234567",
				},
			}
			Expect(k8sClient.Create(ctx, branch)).To(Succeed())

			createdBranch := &terrakojoiov1alpha1.Branch{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(branch), createdBranch)).To(Succeed())

			workflow := &terrakojoiov1alpha1.Workflow{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      workflowName,
					Namespace: namespace,
				},
				Spec: terrakojoiov1alpha1.WorkflowSpec{
					Owner:      createdBranch.Spec.Owner,
					Repository: createdBranch.Spec.Repository,
					Branch:     createdBranch.Name,
					SHA:        createdBranch.Spec.SHA,
					Template:   "tf-workflow-template",
					Path:       "infrastructure/app",
				},
			}
			Expect(controllerutil.SetControllerReference(createdBranch, workflow, mgr.GetScheme())).To(Succeed())
			Expect(k8sClient.Create(ctx, workflow)).To(Succeed())

			createdWorkflow := &terrakojoiov1alpha1.Workflow{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(workflow), createdWorkflow)).To(Succeed())
			createdWorkflow.Status.Phase = string(WorkflowPhaseSucceeded)
			Expect(k8sClient.Status().Update(ctx, createdWorkflow)).To(Succeed())

			Eventually(func() bool {
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(branch), &terrakojoiov1alpha1.Branch{})
				return apierrors.IsNotFound(err)
			}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(BeTrue())

			Eventually(func(g Gomega) {
				fetchedRepo := &terrakojoiov1alpha1.Repository{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(repo), fetchedRepo)).To(Succeed())
				g.Expect(fetchedRepo.Status.BranchList).To(BeEmpty())
			}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
		})

		It("creates Workflow resources for changed files", func() {
			ghManager.GetClientForBranchFunc = func(ctx context.Context, branch *terrakojoiov1alpha1.Branch) (gh.ClientInterface, error) {
				return &fakeGitHubClient{
					GetChangedFilesFunc: func(owner, repo string, prNumber int) ([]string, error) {
						return []string{"infrastructure/app/main.tf", "README.md"}, nil
					},
					GetChangedFilesForCommitFunc: func(owner, repo, sha string) ([]string, error) {
						return []string{"infrastructure/app/variables.tf", "docs/usage.md"}, nil
					},
				}, nil
			}

			ctx := context.Background()
			namespace := createTestNamespace(ctx)
			tfTemplateName := uniqueName("tf-workflow-template")
			mdTemplateName := uniqueName("md-workflow-template")
			branchName := uniqueName("branch-workflow")
			owner := uniqueName("owner")
			repoName := uniqueName("repo")
			tfTemplate := &terrakojoiov1alpha1.WorkflowTemplate{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      tfTemplateName,
					Namespace: namespace,
				},
				Spec: terrakojoiov1alpha1.WorkflowTemplateSpec{
					DisplayName: "tf-test",
					Match: terrakojoiov1alpha1.WorkflowMatch{
						Paths: []string{"infrastructure/**/*.tf"},
					},
					Steps: []terrakojoiov1alpha1.WorkflowStep{
						{
							Name:    "plan",
							Image:   "hashicorp/terraform:latest",
							Command: []string{"echo", "Planning..."},
						},
					},
				},
			}
			mdTemplate := &terrakojoiov1alpha1.WorkflowTemplate{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      mdTemplateName,
					Namespace: namespace,
				},
				Spec: terrakojoiov1alpha1.WorkflowTemplateSpec{
					DisplayName: "md-test",
					Match: terrakojoiov1alpha1.WorkflowMatch{
						Paths: []string{"**/*.md"},
					},
					Steps: []terrakojoiov1alpha1.WorkflowStep{
						{
							Name:    "lint",
							Image:   "markdownlint/markdownlint:latest",
							Command: []string{"echo", "Linting..."},
						},
					},
				},
			}

			Expect(k8sClient.Create(ctx, tfTemplate)).To(Succeed())
			Expect(k8sClient.Create(ctx, mdTemplate)).To(Succeed())

			branch := &terrakojoiov1alpha1.Branch{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      branchName,
					Namespace: namespace,
				},
				Spec: terrakojoiov1alpha1.BranchSpec{
					Repository: repoName,
					Owner:      owner,
					Name:       "feature/workflow",
					SHA:        "0123456789abcdef0123456789abcdef01234567",
				},
			}
			Expect(k8sClient.Create(ctx, branch)).To(Succeed())

			Eventually(func(g Gomega) {
				key := client.ObjectKeyFromObject(branch)
				fetched := &terrakojoiov1alpha1.Branch{}
				g.Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
				g.Expect(fetched.Annotations).To(HaveKeyWithValue("terrakojo.io/last-sha", branch.Spec.SHA))
			}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

			workflowList := &terrakojoiov1alpha1.WorkflowList{}
			Eventually(func(g Gomega) {
				fetchedBranch := &terrakojoiov1alpha1.Branch{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(branch), fetchedBranch)).To(Succeed())
				g.Expect(k8sClient.List(
					ctx,
					workflowList,
					client.InNamespace(namespace),
					client.MatchingLabels{"terrakojo.io/owner-uid": string(fetchedBranch.UID)},
				)).To(Succeed())
				g.Expect(workflowList.Items).To(HaveLen(2))

				for _, wf := range workflowList.Items {
					g.Expect(wf.Labels).To(HaveKeyWithValue("terrakojo.io/owner-uid", string(fetchedBranch.UID)))
					g.Expect(wf.Spec.Branch).To(Equal(branch.Name))
					g.Expect(wf.Spec.Owner).To(Equal(branch.Spec.Owner))
					g.Expect(wf.Spec.Repository).To(Equal(branch.Spec.Repository))
					g.Expect(wf.Spec.Template).To(SatisfyAny(
						Equal(tfTemplateName),
						Equal(mdTemplateName),
					))
					if wf.Spec.Template == tfTemplateName {
						g.Expect(wf.Spec.Path).To(Equal("infrastructure/app"))
					}
					if wf.Spec.Template == mdTemplateName {
						g.Expect(wf.Spec.Path).To(Equal("docs"))
					}
				}
			}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
		})

		It("recreates Workflow resources if chaged branch SHA", func() {
			var getChangedFilesCalled atomic.Bool
			ghManager.GetClientForBranchFunc = func(ctx context.Context, branch *terrakojoiov1alpha1.Branch) (gh.ClientInterface, error) {
				return &fakeGitHubClient{
					GetChangedFilesForCommitFunc: func(owner, repo, sha string) ([]string, error) {
						getChangedFilesCalled.Store(true)
						return []string{"infrastructure/app/main.tf"}, nil
					},
				}, nil
			}

			ctx := context.Background()
			namespace := createTestNamespace(ctx)
			templateName := uniqueName("tf-workflow-template-recreate")
			branchName := uniqueName("branch-recreate-workflow")
			owner := uniqueName("owner")
			repoName := uniqueName("repo")
			workflowTemplate := &terrakojoiov1alpha1.WorkflowTemplate{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      templateName,
					Namespace: namespace,
				},
				Spec: terrakojoiov1alpha1.WorkflowTemplateSpec{
					DisplayName: "tf-test",
					Match: terrakojoiov1alpha1.WorkflowMatch{
						Paths: []string{"infrastructure/**/*.tf"},
					},
					Steps: []terrakojoiov1alpha1.WorkflowStep{
						{
							Name:    "plan",
							Image:   "hashicorp/terraform:latest",
							Command: []string{"echo", "Planning..."},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, workflowTemplate)).To(Succeed())

			branch := &terrakojoiov1alpha1.Branch{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      branchName,
					Namespace: namespace,
					Annotations: map[string]string{
						"terrakojo.io/last-sha": "0123456789abcdef0123456789abcdef01234567",
					},
				},
				Spec: terrakojoiov1alpha1.BranchSpec{
					Repository: repoName,
					Owner:      owner,
					Name:       "feature/recreate-workflow",
					SHA:        "0123456789abcdef0123456789abcdef01234568",
				},
			}
			Expect(k8sClient.Create(ctx, branch)).To(Succeed())

			Eventually(getChangedFilesCalled.Load).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(BeTrue())

			Eventually(func(g Gomega) {
				key := client.ObjectKeyFromObject(branch)
				fetched := &terrakojoiov1alpha1.Branch{}
				g.Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
				g.Expect(fetched.Annotations).To(HaveKeyWithValue("terrakojo.io/last-sha", branch.Spec.SHA))
			}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

			workflowList := &terrakojoiov1alpha1.WorkflowList{}
			Eventually(func(g Gomega) {
				fetchedBranch := &terrakojoiov1alpha1.Branch{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(branch), fetchedBranch)).To(Succeed())
				g.Expect(k8sClient.List(
					ctx,
					workflowList,
					client.InNamespace(namespace),
					client.MatchingLabels{"terrakojo.io/owner-uid": string(fetchedBranch.UID)},
				)).To(Succeed())
				g.Expect(workflowList.Items).To(HaveLen(1))

				wf := workflowList.Items[0]
				g.Expect(wf.Labels).To(HaveKeyWithValue("terrakojo.io/owner-uid", string(fetchedBranch.UID)))
				g.Expect(wf.Spec.Branch).To(Equal(branch.Name))
				g.Expect(wf.Spec.Owner).To(Equal(branch.Spec.Owner))
				g.Expect(wf.Spec.Repository).To(Equal(branch.Spec.Repository))
			}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
		})

		It("uses PR changed files API when PRNumber is set", func() {
			var prCalled atomic.Bool
			var commitCalled atomic.Bool
			ghManager.GetClientForBranchFunc = func(ctx context.Context, branch *terrakojoiov1alpha1.Branch) (gh.ClientInterface, error) {
				return &fakeGitHubClient{
					GetChangedFilesFunc: func(owner, repo string, prNumber int) ([]string, error) {
						prCalled.Store(true)
						return []string{}, nil
					},
					GetChangedFilesForCommitFunc: func(owner, repo, sha string) ([]string, error) {
						commitCalled.Store(true)
						return []string{}, nil
					},
				}, nil
			}

			ctx := context.Background()
			namespace := createTestNamespace(ctx)
			branchName := uniqueName("branch-pr")
			owner := uniqueName("owner")
			repoName := uniqueName("repo")
			branch := &terrakojoiov1alpha1.Branch{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      branchName,
					Namespace: namespace,
				},
				Spec: terrakojoiov1alpha1.BranchSpec{
					Repository: repoName,
					Owner:      owner,
					Name:       "feature/pr",
					PRNumber:   123,
					SHA:        "0123456789abcdef0123456789abcdef01234567",
				},
			}
			Expect(k8sClient.Create(ctx, branch)).To(Succeed())

			Eventually(prCalled.Load).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(BeTrue())
			Expect(commitCalled.Load()).To(BeFalse())

			Eventually(func() bool {
				key := client.ObjectKeyFromObject(branch)
				err := k8sClient.Get(ctx, key, &terrakojoiov1alpha1.Branch{})
				return apierrors.IsNotFound(err)
			}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(BeTrue())
		})

		It("no matching WorkflowTemplates results in no Workflows created", func() {
			ghManager.GetClientForBranchFunc = func(ctx context.Context, branch *terrakojoiov1alpha1.Branch) (gh.ClientInterface, error) {
				return &fakeGitHubClient{
					GetChangedFilesForCommitFunc: func(owner, repo, sha string) ([]string, error) {
						return []string{"some/random/file.txt"}, nil
					},
				}, nil
			}

			ctx := context.Background()
			namespace := createTestNamespace(ctx)
			templateName := uniqueName("tf-workflow-template")
			branchName := uniqueName("branch-no-workflow")
			owner := uniqueName("owner")
			repoName := uniqueName("repo")
			workflowTemplate := &terrakojoiov1alpha1.WorkflowTemplate{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      templateName,
					Namespace: namespace,
				},
				Spec: terrakojoiov1alpha1.WorkflowTemplateSpec{
					DisplayName: "tf-test",
					Match: terrakojoiov1alpha1.WorkflowMatch{
						Paths: []string{"infrastructure/**/*.tf"},
					},
					Steps: []terrakojoiov1alpha1.WorkflowStep{
						{
							Name:    "plan",
							Image:   "hashicorp/terraform:latest",
							Command: []string{"echo", "Planning..."},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, workflowTemplate)).To(Succeed())

			branch := &terrakojoiov1alpha1.Branch{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      branchName,
					Namespace: namespace,
				},
				Spec: terrakojoiov1alpha1.BranchSpec{
					Repository: repoName,
					Owner:      owner,
					Name:       "feature/no-workflow",
					SHA:        "0123456789abcdef0123456789abcdef01234567",
				},
			}
			Expect(k8sClient.Create(ctx, branch)).To(Succeed())

			Eventually(func() bool {
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(branch), &terrakojoiov1alpha1.Branch{})
				return apierrors.IsNotFound(err)
			}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(BeTrue())
		})
	})

	When("reconciling Branch resources (error paths)", func() {
		type setupResult struct {
			reconciler *BranchReconciler
			request    ctrl.Request
		}

		makeRequest := func(branch *terrakojoiov1alpha1.Branch) ctrl.Request {
			return ctrl.Request{NamespacedName: types.NamespacedName{Name: branch.Name, Namespace: branch.Namespace}}
		}

		DescribeTable("returns an error",
			func(setup func() setupResult, expectedSubstring string) {
				result := setup()
				_, err := result.reconciler.Reconcile(context.Background(), result.request)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring(expectedSubstring))
			},
			Entry("fails when listing workflows errors", func() setupResult {
				scheme := newBranchTestScheme()
				branch := newErrorTestBranch()
				baseClient := newBranchFakeClient(scheme, branch)
				reconciler := &BranchReconciler{
					Client:              &listErrorClient{Client: baseClient, err: fmt.Errorf("list failed")},
					Scheme:              scheme,
					GitHubClientManager: &fakeGitHubClientManager{},
				}
				return setupResult{reconciler: reconciler, request: makeRequest(branch)}
			}, "list failed"),
			Entry("fails when GitHubClientManager is nil", func() setupResult {
				scheme := newBranchTestScheme()
				branch := newErrorTestBranch()
				client := newBranchFakeClient(scheme, branch)
				reconciler := &BranchReconciler{
					Client:              client,
					Scheme:              scheme,
					GitHubClientManager: nil,
				}
				return setupResult{reconciler: reconciler, request: makeRequest(branch)}
			}, "GitHubClientManager not initialized"),
			Entry("fails when GetClientForBranch returns error", func() setupResult {
				scheme := newBranchTestScheme()
				branch := newErrorTestBranch()
				client := newBranchFakeClient(scheme, branch)
				ghManager := &fakeGitHubClientManager{
					GetClientForBranchFunc: func(ctx context.Context, branch *terrakojoiov1alpha1.Branch) (gh.ClientInterface, error) {
						return nil, fmt.Errorf("gh client error")
					},
				}
				reconciler := &BranchReconciler{
					Client:              client,
					Scheme:              scheme,
					GitHubClientManager: ghManager,
				}
				return setupResult{reconciler: reconciler, request: makeRequest(branch)}
			}, "gh client error"),
			Entry("fails when commit changed files returns error", func() setupResult {
				scheme := newBranchTestScheme()
				branch := newErrorTestBranch()
				client := newBranchFakeClient(scheme, branch)
				ghManager := &fakeGitHubClientManager{
					GetClientForBranchFunc: func(ctx context.Context, branch *terrakojoiov1alpha1.Branch) (gh.ClientInterface, error) {
						return &fakeGitHubClient{
							GetChangedFilesForCommitFunc: func(owner, repo, sha string) ([]string, error) {
								return nil, fmt.Errorf("changed files error")
							},
						}, nil
					},
				}
				reconciler := &BranchReconciler{
					Client:              client,
					Scheme:              scheme,
					GitHubClientManager: ghManager,
				}
				return setupResult{reconciler: reconciler, request: makeRequest(branch)}
			}, "changed files error"),
			Entry("fails when listing workflow templates returns error", func() setupResult {
				scheme := newBranchTestScheme()
				branch := newErrorTestBranch()
				baseClient := newBranchFakeClient(scheme, branch)
				ghManager := &fakeGitHubClientManager{
					GetClientForBranchFunc: func(ctx context.Context, branch *terrakojoiov1alpha1.Branch) (gh.ClientInterface, error) {
						return &fakeGitHubClient{
							GetChangedFilesForCommitFunc: func(owner, repo, sha string) ([]string, error) {
								return []string{"infrastructure/app/main.tf"}, nil
							},
						}, nil
					},
				}
				reconciler := &BranchReconciler{
					Client:              &listWorkflowTemplateErrorClient{Client: baseClient, err: fmt.Errorf("list templates failed")},
					Scheme:              scheme,
					GitHubClientManager: ghManager,
				}
				return setupResult{reconciler: reconciler, request: makeRequest(branch)}
			}, "list templates failed"),
			Entry("fails when adding finalizer update errors", func() setupResult {
				scheme := newBranchTestScheme()
				branch := newErrorTestBranch()
				branch.Finalizers = nil
				baseClient := newBranchFakeClient(scheme, branch)
				reconciler := &BranchReconciler{
					Client:              &updateErrorClient{Client: baseClient, err: fmt.Errorf("update failed")},
					Scheme:              scheme,
					GitHubClientManager: &fakeGitHubClientManager{},
				}
				return setupResult{reconciler: reconciler, request: makeRequest(branch)}
			}, "update failed"),
			Entry("fails when deleting workflows errors during finalization", func() setupResult {
				scheme := newBranchTestScheme()
				branch := newErrorTestBranch()
				now := metav1.Now()
				branch.DeletionTimestamp = &now
				branch.UID = types.UID("branch-error-uid")
				workflow := &terrakojoiov1alpha1.Workflow{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "workflow-error",
						Namespace: branch.Namespace,
					},
					Spec: terrakojoiov1alpha1.WorkflowSpec{
						Owner:      branch.Spec.Owner,
						Repository: branch.Spec.Repository,
						Branch:     branch.Name,
						SHA:        branch.Spec.SHA,
						Template:   "template",
					},
				}
				Expect(controllerutil.SetControllerReference(branch, workflow, scheme)).To(Succeed())
				baseClient := newBranchFakeClient(scheme, branch, workflow)
				reconciler := &BranchReconciler{
					Client:              &deleteWorkflowErrorClient{Client: baseClient, err: fmt.Errorf("delete workflow error")},
					Scheme:              scheme,
					GitHubClientManager: &fakeGitHubClientManager{},
				}
				return setupResult{reconciler: reconciler, request: makeRequest(branch)}
			}, "failed to delete workflow"),
		)
	})
})
