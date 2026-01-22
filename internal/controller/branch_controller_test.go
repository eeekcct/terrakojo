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
	"sync/atomic"
	"time"

	terrakojoiov1alpha1 "github.com/eeekcct/terrakojo/api/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	gh "github.com/eeekcct/terrakojo/internal/github"
)

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
		const namespace = "default"

		It("add finalizer on creation", func() {
			ctx := context.Background()
			branch := &terrakojoiov1alpha1.Branch{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      "example-branch-finalizer",
					Namespace: namespace,
					Annotations: map[string]string{
						"terrakojo.io/last-sha": "0123456789abcdef0123456789abcdef01234567",
					},
				},
				Spec: terrakojoiov1alpha1.BranchSpec{
					Repository: "example-repo",
					Owner:      "example-owner",
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
			branch := &terrakojoiov1alpha1.Branch{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      "example-branch-delete-requeue",
					Namespace: namespace,
					Finalizers: []string{
						branchFinalizer,
					},
					Annotations: map[string]string{
						"terrakojo.io/last-sha": "0123456789abcdef0123456789abcdef01234567",
					},
				},
				Spec: terrakojoiov1alpha1.BranchSpec{
					Repository: "example-repo",
					Owner:      "example-owner",
					Name:       "feature/requeue",
					SHA:        "0123456789abcdef0123456789abcdef01234567",
				},
			}
			Expect(k8sClient.Create(ctx, branch)).To(Succeed())

			createdBranch := &terrakojoiov1alpha1.Branch{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(branch), createdBranch)).To(Succeed())

			workflow := &terrakojoiov1alpha1.Workflow{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      "example-workflow-remains",
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
			branch := &terrakojoiov1alpha1.Branch{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      "example-branch-delete-clean",
					Namespace: namespace,
					Finalizers: []string{
						branchFinalizer,
					},
					Annotations: map[string]string{
						"terrakojo.io/last-sha": "abcdef0123456789abcdef0123456789abcdef01",
					},
				},
				Spec: terrakojoiov1alpha1.BranchSpec{
					Repository: "example-repo",
					Owner:      "example-owner",
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
			repo := &terrakojoiov1alpha1.Repository{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      "example-repo-cleanup",
					Namespace: namespace,
					Labels: map[string]string{
						"terrakojo.io/owner":     "example-owner",
						"terrakojo.io/repo-name": "example-repo",
					},
				},
				Spec: terrakojoiov1alpha1.RepositorySpec{
					Owner:         "example-owner",
					Name:          "example-repo",
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
					Ref: "feature/completed",
					SHA: "0123456789abcdef0123456789abcdef01234567",
				},
			}
			Expect(k8sClient.Status().Update(ctx, createdRepo)).To(Succeed())

			branch := &terrakojoiov1alpha1.Branch{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      "example-branch-completed",
					Namespace: namespace,
					Finalizers: []string{
						branchFinalizer,
					},
					Annotations: map[string]string{
						"terrakojo.io/last-sha": "0123456789abcdef0123456789abcdef01234567",
					},
				},
				Spec: terrakojoiov1alpha1.BranchSpec{
					Repository: "example-repo",
					Owner:      "example-owner",
					Name:       "feature/completed",
					SHA:        "0123456789abcdef0123456789abcdef01234567",
				},
			}
			Expect(k8sClient.Create(ctx, branch)).To(Succeed())

			createdBranch := &terrakojoiov1alpha1.Branch{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(branch), createdBranch)).To(Succeed())

			workflow := &terrakojoiov1alpha1.Workflow{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      "example-workflow-completed",
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
			tfTemplate := &terrakojoiov1alpha1.WorkflowTemplate{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      "tf-workflow-template",
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
					Name:      "md-workflow-template",
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
					Name:      "example-branch-workflow",
					Namespace: namespace,
				},
				Spec: terrakojoiov1alpha1.BranchSpec{
					Repository: "example-repo",
					Owner:      "example-owner",
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
						Equal("tf-workflow-template"),
						Equal("md-workflow-template"),
					))
					if wf.Spec.Template == "tf-workflow-template" {
						g.Expect(wf.Spec.Path).To(Equal("infrastructure/app"))
					}
					if wf.Spec.Template == "md-workflow-template" {
						g.Expect(wf.Spec.Path).To(Equal("docs"))
					}
				}
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
			branch := &terrakojoiov1alpha1.Branch{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      "example-branch",
					Namespace: namespace,
				},
				Spec: terrakojoiov1alpha1.BranchSpec{
					Repository: "example-repo",
					Owner:      "example-owner",
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
	})
})
