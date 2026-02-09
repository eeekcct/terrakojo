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
	batchv1 "k8s.io/api/batch/v1"
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

const defaultBranchName = "main"

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

func newBranchRepository(namespace, owner, repoName string) *terrakojoiov1alpha1.Repository {
	return &terrakojoiov1alpha1.Repository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      repoName,
			Namespace: namespace,
		},
		Spec: terrakojoiov1alpha1.RepositorySpec{
			Owner:         owner,
			Name:          repoName,
			Type:          "github",
			DefaultBranch: defaultBranchName,
			GitHubSecretRef: terrakojoiov1alpha1.GitHubSecretRef{
				Name: "dummy",
			},
		},
	}
}

func repoForBranch(branch *terrakojoiov1alpha1.Branch) *terrakojoiov1alpha1.Repository {
	return newBranchRepository(branch.Namespace, branch.Spec.Owner, branch.Spec.Repository)
}

func repositoryOwnerReference(repoName string, repoUID types.UID) metav1.OwnerReference {
	controller := true
	blockOwnerDeletion := true
	return metav1.OwnerReference{
		APIVersion:         terrakojoiov1alpha1.GroupVersion.String(),
		Kind:               "Repository",
		Name:               repoName,
		UID:                repoUID,
		Controller:         &controller,
		BlockOwnerDeletion: &blockOwnerDeletion,
	}
}

func attachRepositoryOwnerReference(branch *terrakojoiov1alpha1.Branch, repo *terrakojoiov1alpha1.Repository) {
	branch.OwnerReferences = []metav1.OwnerReference{
		repositoryOwnerReference(repo.Name, repo.UID),
	}
}

func newBranchTestScheme() *runtime.Scheme {
	testScheme := runtime.NewScheme()
	Expect(terrakojoiov1alpha1.AddToScheme(testScheme)).To(Succeed())
	return testScheme
}

func newBranchFakeClient(testScheme *runtime.Scheme, objs ...client.Object) client.Client {
	builder := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithIndex(&terrakojoiov1alpha1.Workflow{}, "metadata.ownerReferences.uid", indexByOwnerBranchUID).
		WithStatusSubresource(&terrakojoiov1alpha1.Branch{}, &terrakojoiov1alpha1.Repository{}, &terrakojoiov1alpha1.Workflow{})
	if len(objs) > 0 {
		builder.WithObjects(objs...)
	}
	return builder.Build()
}

func newBranchTemplateJobSpec(containerName, image string, command []string) batchv1.JobSpec {
	return batchv1.JobSpec{
		Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Containers: []corev1.Container{
					{
						Name:    containerName,
						Image:   image,
						Command: command,
					},
				},
			},
		},
	}
}

func newErrorTestBranch() *terrakojoiov1alpha1.Branch {
	return &terrakojoiov1alpha1.Branch{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "branch-error-test",
			Namespace:  "default",
			Finalizers: []string{branchFinalizer},
			OwnerReferences: []metav1.OwnerReference{
				repositoryOwnerReference("repo", ""),
			},
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

type workflowListSequenceClient struct {
	client.Client
	err   error
	calls int
}

func (c *workflowListSequenceClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*terrakojoiov1alpha1.WorkflowList); ok {
		c.calls++
		if c.calls == 2 {
			return c.err
		}
	}
	return c.Client.List(ctx, list, opts...)
}

type createWorkflowErrorClient struct {
	client.Client
	err error
}

func (c *createWorkflowErrorClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, ok := obj.(*terrakojoiov1alpha1.Workflow); ok {
		return c.err
	}
	return c.Client.Create(ctx, obj, opts...)
}

type statusErrorClient struct {
	client.Client
	err error
}

type statusErrorWriter struct {
	client.SubResourceWriter
	err error
}

func (c *statusErrorClient) Status() client.SubResourceWriter {
	return &statusErrorWriter{SubResourceWriter: c.Client.Status(), err: c.err}
}

func (w *statusErrorWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	return w.err
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
			repo := newBranchRepository(namespace, owner, repoName)
			Expect(k8sClient.Create(ctx, repo)).To(Succeed())
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
			attachRepositoryOwnerReference(branch, repo)
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
			repo := newBranchRepository(namespace, owner, repoName)
			Expect(k8sClient.Create(ctx, repo)).To(Succeed())
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
			attachRepositoryOwnerReference(branch, repo)
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
			repo := newBranchRepository(namespace, owner, repoName)
			Expect(k8sClient.Create(ctx, repo)).To(Succeed())
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
			attachRepositoryOwnerReference(branch, repo)
			Expect(k8sClient.Create(ctx, branch)).To(Succeed())
			Expect(k8sClient.Delete(ctx, branch)).To(Succeed())

			Eventually(func() bool {
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(branch), &terrakojoiov1alpha1.Branch{})
				return apierrors.IsNotFound(err)
			}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(BeTrue())
		})

		It("keeps non-default branch when all workflows completed", func() {
			ctx := context.Background()
			namespace := createTestNamespace(ctx)
			owner := uniqueName("owner")
			repoName := uniqueName("repo")
			branchName := uniqueName("branch-completed")
			workflowName := uniqueName("workflow-completed")
			branchRef := "feature/completed"
			repo := &terrakojoiov1alpha1.Repository{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      repoName,
					Namespace: namespace,
				},
				Spec: terrakojoiov1alpha1.RepositorySpec{
					Owner:         owner,
					Name:          repoName,
					Type:          "github",
					DefaultBranch: defaultBranchName,
					GitHubSecretRef: terrakojoiov1alpha1.GitHubSecretRef{
						Name: "dummy",
					},
				},
			}
			Expect(k8sClient.Create(ctx, repo)).To(Succeed())

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
			attachRepositoryOwnerReference(branch, repo)
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

			Eventually(func(g Gomega) {
				fetched := &terrakojoiov1alpha1.Branch{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(branch), fetched)).To(Succeed())
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
			repo := newBranchRepository(namespace, owner, repoName)
			Expect(k8sClient.Create(ctx, repo)).To(Succeed())
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
					Job: newBranchTemplateJobSpec("plan", "hashicorp/terraform:latest", []string{"echo", "Planning..."}),
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
					Job: newBranchTemplateJobSpec("lint", "markdownlint/markdownlint:latest", []string{"echo", "Linting..."}),
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
			attachRepositoryOwnerReference(branch, repo)
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
					g.Expect(wf.Spec.Parameters).To(HaveKeyWithValue("isDefaultBranch", "false"))
					g.Expect(wf.Spec.Parameters).To(HaveKeyWithValue("executionUnit", "folder"))
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

		It("creates one Workflow per repository when executionUnit is repository", func() {
			ghManager.GetClientForBranchFunc = func(ctx context.Context, branch *terrakojoiov1alpha1.Branch) (gh.ClientInterface, error) {
				return &fakeGitHubClient{
					GetChangedFilesForCommitFunc: func(owner, repo, sha string) ([]string, error) {
						return []string{"infrastructure/app/main.tf", "infrastructure/db/variables.tf"}, nil
					},
				}, nil
			}

			ctx := context.Background()
			namespace := createTestNamespace(ctx)
			templateName := uniqueName("repo-workflow-template")
			branchName := uniqueName("branch-repo-unit")
			owner := uniqueName("owner")
			repoName := uniqueName("repo")
			repo := newBranchRepository(namespace, owner, repoName)
			Expect(k8sClient.Create(ctx, repo)).To(Succeed())

			workflowTemplate := &terrakojoiov1alpha1.WorkflowTemplate{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      templateName,
					Namespace: namespace,
				},
				Spec: terrakojoiov1alpha1.WorkflowTemplateSpec{
					DisplayName: "repo-unit",
					Match: terrakojoiov1alpha1.WorkflowMatch{
						Paths:         []string{"infrastructure/**/*.tf"},
						ExecutionUnit: terrakojoiov1alpha1.WorkflowExecutionUnitRepository,
					},
					Job: newBranchTemplateJobSpec("plan", "hashicorp/terraform:latest", []string{"echo", "Planning..."}),
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
					Name:       "feature/repo-unit",
					SHA:        "0123456789abcdef0123456789abcdef01234567",
				},
			}
			attachRepositoryOwnerReference(branch, repo)
			Expect(k8sClient.Create(ctx, branch)).To(Succeed())

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
				g.Expect(wf.Spec.Path).To(Equal("."))
				g.Expect(wf.Spec.Template).To(Equal(templateName))
				g.Expect(wf.Spec.Parameters).To(HaveKeyWithValue("executionUnit", "repository"))
				g.Expect(wf.Spec.Parameters).To(HaveKeyWithValue("isDefaultBranch", "false"))
			}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
		})

		It("creates one Workflow per file when executionUnit is file", func() {
			ghManager.GetClientForBranchFunc = func(ctx context.Context, branch *terrakojoiov1alpha1.Branch) (gh.ClientInterface, error) {
				return &fakeGitHubClient{
					GetChangedFilesForCommitFunc: func(owner, repo, sha string) ([]string, error) {
						return []string{
							"infrastructure/app/main.tf",
							"infrastructure/db/variables.tf",
							"infrastructure/db/variables.tf",
						}, nil
					},
				}, nil
			}

			ctx := context.Background()
			namespace := createTestNamespace(ctx)
			templateName := uniqueName("file-workflow-template")
			branchName := uniqueName("branch-file-unit")
			owner := uniqueName("owner")
			repoName := uniqueName("repo")
			repo := newBranchRepository(namespace, owner, repoName)
			Expect(k8sClient.Create(ctx, repo)).To(Succeed())

			workflowTemplate := &terrakojoiov1alpha1.WorkflowTemplate{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      templateName,
					Namespace: namespace,
				},
				Spec: terrakojoiov1alpha1.WorkflowTemplateSpec{
					DisplayName: "file-unit",
					Match: terrakojoiov1alpha1.WorkflowMatch{
						Paths:         []string{"infrastructure/**/*.tf"},
						ExecutionUnit: terrakojoiov1alpha1.WorkflowExecutionUnitFile,
					},
					Job: newBranchTemplateJobSpec("plan", "hashicorp/terraform:latest", []string{"echo", "Planning..."}),
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
					Name:       "feature/file-unit",
					SHA:        "0123456789abcdef0123456789abcdef01234567",
				},
			}
			attachRepositoryOwnerReference(branch, repo)
			Expect(k8sClient.Create(ctx, branch)).To(Succeed())

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

				paths := make([]string, 0, len(workflowList.Items))
				for _, wf := range workflowList.Items {
					g.Expect(wf.Spec.Template).To(Equal(templateName))
					g.Expect(wf.Spec.Parameters).To(HaveKeyWithValue("executionUnit", "file"))
					g.Expect(wf.Spec.Parameters).To(HaveKeyWithValue("isDefaultBranch", "false"))
					paths = append(paths, wf.Spec.Path)
				}
				g.Expect(paths).To(ConsistOf(
					"infrastructure/app/main.tf",
					"infrastructure/db/variables.tf",
				))
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
			repo := newBranchRepository(namespace, owner, repoName)
			Expect(k8sClient.Create(ctx, repo)).To(Succeed())
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
					Job: newBranchTemplateJobSpec("plan", "hashicorp/terraform:latest", []string{"echo", "Planning..."}),
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
			attachRepositoryOwnerReference(branch, repo)
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

		It("recreates workflows when SHA changes after completion", func() {
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
			templateName := uniqueName("tf-workflow-template-completed")
			branchName := uniqueName("branch-recreate-completed")
			owner := uniqueName("owner")
			repoName := uniqueName("repo")
			repo := newBranchRepository(namespace, owner, repoName)
			Expect(k8sClient.Create(ctx, repo)).To(Succeed())
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
					Job: newBranchTemplateJobSpec("plan", "hashicorp/terraform:latest", []string{"echo", "Planning..."}),
				},
			}
			Expect(k8sClient.Create(ctx, workflowTemplate)).To(Succeed())

			oldSHA := "0123456789abcdef0123456789abcdef01234567"
			newSHA := "0123456789abcdef0123456789abcdef01234568"
			branch := &terrakojoiov1alpha1.Branch{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      branchName,
					Namespace: namespace,
					Annotations: map[string]string{
						"terrakojo.io/last-sha": oldSHA,
					},
				},
				Spec: terrakojoiov1alpha1.BranchSpec{
					Repository: repoName,
					Owner:      owner,
					Name:       "feature/recreate-completed",
					SHA:        oldSHA,
				},
			}
			attachRepositoryOwnerReference(branch, repo)
			Expect(k8sClient.Create(ctx, branch)).To(Succeed())

			createdBranch := &terrakojoiov1alpha1.Branch{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(branch), createdBranch)).To(Succeed())

			workflow := &terrakojoiov1alpha1.Workflow{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      uniqueName("workflow-completed"),
					Namespace: namespace,
				},
				Spec: terrakojoiov1alpha1.WorkflowSpec{
					Owner:      createdBranch.Spec.Owner,
					Repository: createdBranch.Spec.Repository,
					Branch:     createdBranch.Name,
					SHA:        oldSHA,
					Template:   templateName,
					Path:       "infrastructure/app",
				},
			}
			Expect(controllerutil.SetControllerReference(createdBranch, workflow, mgr.GetScheme())).To(Succeed())
			Expect(k8sClient.Create(ctx, workflow)).To(Succeed())

			createdWorkflow := &terrakojoiov1alpha1.Workflow{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(workflow), createdWorkflow)).To(Succeed())
			createdWorkflow.Status.Phase = string(WorkflowPhaseSucceeded)
			Expect(k8sClient.Status().Update(ctx, createdWorkflow)).To(Succeed())

			Eventually(func(g Gomega) {
				fetched := &terrakojoiov1alpha1.Branch{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(branch), fetched)).To(Succeed())
				fetched.Spec.SHA = newSHA
				g.Expect(k8sClient.Update(ctx, fetched)).To(Succeed())
			}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())

			Eventually(getChangedFilesCalled.Load).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(BeTrue())

			Eventually(func(g Gomega) {
				fetchedBranch := &terrakojoiov1alpha1.Branch{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(branch), fetchedBranch)).To(Succeed())
				g.Expect(fetchedBranch.Annotations).To(HaveKeyWithValue("terrakojo.io/last-sha", newSHA))

				workflowList := &terrakojoiov1alpha1.WorkflowList{}
				g.Expect(k8sClient.List(
					ctx,
					workflowList,
					client.InNamespace(namespace),
					client.MatchingLabels{"terrakojo.io/owner-uid": string(fetchedBranch.UID)},
				)).To(Succeed())
				g.Expect(workflowList.Items).To(HaveLen(1))
				g.Expect(workflowList.Items[0].Spec.SHA).To(Equal(newSHA))
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
			repo := newBranchRepository(namespace, owner, repoName)
			Expect(k8sClient.Create(ctx, repo)).To(Succeed())
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
			attachRepositoryOwnerReference(branch, repo)
			Expect(k8sClient.Create(ctx, branch)).To(Succeed())

			Eventually(prCalled.Load).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(BeTrue())
			Expect(commitCalled.Load()).To(BeFalse())

			Eventually(func(g Gomega) {
				key := client.ObjectKeyFromObject(branch)
				fetched := &terrakojoiov1alpha1.Branch{}
				g.Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
				g.Expect(fetched.Annotations).To(HaveKeyWithValue("terrakojo.io/last-sha", branch.Spec.SHA))
			}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
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
			repo := newBranchRepository(namespace, owner, repoName)
			Expect(k8sClient.Create(ctx, repo)).To(Succeed())
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
					Job: newBranchTemplateJobSpec("plan", "hashicorp/terraform:latest", []string{"echo", "Planning..."}),
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
			attachRepositoryOwnerReference(branch, repo)
			Expect(k8sClient.Create(ctx, branch)).To(Succeed())

			Eventually(func(g Gomega) {
				fetched := &terrakojoiov1alpha1.Branch{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(branch), fetched)).To(Succeed())
				g.Expect(fetched.Annotations).To(HaveKeyWithValue("terrakojo.io/last-sha", branch.Spec.SHA))
			}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
		})
	})

	When("reconciling Branch resources (unit paths)", func() {
		It("skips workflow creation when matched files are at repo root", func() {
			testScheme := newBranchTestScheme()
			branch := newErrorTestBranch()
			template := &terrakojoiov1alpha1.WorkflowTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "template-root-file",
					Namespace: branch.Namespace,
				},
				Spec: terrakojoiov1alpha1.WorkflowTemplateSpec{
					DisplayName: "root-file",
					Match: terrakojoiov1alpha1.WorkflowMatch{
						Paths: []string{"**/*.md"},
					},
					Job: newBranchTemplateJobSpec("lint", "markdownlint/markdownlint:latest", []string{"echo", "Linting..."}),
				},
			}
			repo := repoForBranch(branch)
			fakeClient := newBranchFakeClient(testScheme, branch, repo, template)
			ghManager := &fakeGitHubClientManager{
				GetClientForBranchFunc: func(ctx context.Context, branch *terrakojoiov1alpha1.Branch) (gh.ClientInterface, error) {
					return &fakeGitHubClient{
						GetChangedFilesForCommitFunc: func(owner, repo, sha string) ([]string, error) {
							return []string{"README.md"}, nil
						},
					}, nil
				},
			}
			reconciler := &BranchReconciler{
				Client:              fakeClient,
				Scheme:              testScheme,
				GitHubClientManager: ghManager,
			}

			_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: branch.Name, Namespace: branch.Namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			workflowList := &terrakojoiov1alpha1.WorkflowList{}
			Expect(fakeClient.List(context.Background(), workflowList, client.InNamespace(branch.Namespace))).To(Succeed())
			Expect(workflowList.Items).To(BeEmpty())
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
				testScheme := newBranchTestScheme()
				branch := newErrorTestBranch()
				repo := repoForBranch(branch)
				baseClient := newBranchFakeClient(testScheme, branch, repo)
				reconciler := &BranchReconciler{
					Client:              &listErrorClient{Client: baseClient, err: fmt.Errorf("list failed")},
					Scheme:              testScheme,
					GitHubClientManager: &fakeGitHubClientManager{},
				}
				return setupResult{reconciler: reconciler, request: makeRequest(branch)}
			}, "list failed"),
			Entry("fails when GitHubClientManager is nil", func() setupResult {
				testScheme := newBranchTestScheme()
				branch := newErrorTestBranch()
				repo := repoForBranch(branch)
				client := newBranchFakeClient(testScheme, branch, repo)
				reconciler := &BranchReconciler{
					Client:              client,
					Scheme:              testScheme,
					GitHubClientManager: nil,
				}
				return setupResult{reconciler: reconciler, request: makeRequest(branch)}
			}, "GitHubClientManager not initialized"),
			Entry("fails when GetClientForBranch returns error", func() setupResult {
				testScheme := newBranchTestScheme()
				branch := newErrorTestBranch()
				repo := repoForBranch(branch)
				client := newBranchFakeClient(testScheme, branch, repo)
				ghManager := &fakeGitHubClientManager{
					GetClientForBranchFunc: func(ctx context.Context, branch *terrakojoiov1alpha1.Branch) (gh.ClientInterface, error) {
						return nil, fmt.Errorf("gh client error")
					},
				}
				reconciler := &BranchReconciler{
					Client:              client,
					Scheme:              testScheme,
					GitHubClientManager: ghManager,
				}
				return setupResult{reconciler: reconciler, request: makeRequest(branch)}
			}, "gh client error"),
			Entry("fails when commit changed files returns error", func() setupResult {
				testScheme := newBranchTestScheme()
				branch := newErrorTestBranch()
				repo := repoForBranch(branch)
				client := newBranchFakeClient(testScheme, branch, repo)
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
					Scheme:              testScheme,
					GitHubClientManager: ghManager,
				}
				return setupResult{reconciler: reconciler, request: makeRequest(branch)}
			}, "changed files error"),
			Entry("fails when listing workflow templates returns error", func() setupResult {
				testScheme := newBranchTestScheme()
				branch := newErrorTestBranch()
				repo := repoForBranch(branch)
				baseClient := newBranchFakeClient(testScheme, branch, repo)
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
					Scheme:              testScheme,
					GitHubClientManager: ghManager,
				}
				return setupResult{reconciler: reconciler, request: makeRequest(branch)}
			}, "list templates failed"),
			Entry("fails when adding finalizer update errors", func() setupResult {
				testScheme := newBranchTestScheme()
				branch := newErrorTestBranch()
				branch.Finalizers = nil
				baseClient := newBranchFakeClient(testScheme, branch)
				reconciler := &BranchReconciler{
					Client:              &updateErrorClient{Client: baseClient, err: fmt.Errorf("update failed")},
					Scheme:              testScheme,
					GitHubClientManager: &fakeGitHubClientManager{},
				}
				return setupResult{reconciler: reconciler, request: makeRequest(branch)}
			}, "update failed"),
			Entry("fails when deleting workflows errors during finalization", func() setupResult {
				testScheme := newBranchTestScheme()
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
				Expect(controllerutil.SetControllerReference(branch, workflow, testScheme)).To(Succeed())
				baseClient := newBranchFakeClient(testScheme, branch, workflow)
				reconciler := &BranchReconciler{
					Client:              &deleteWorkflowErrorClient{Client: baseClient, err: fmt.Errorf("delete workflow error")},
					Scheme:              testScheme,
					GitHubClientManager: &fakeGitHubClientManager{},
				}
				return setupResult{reconciler: reconciler, request: makeRequest(branch)}
			}, "failed to delete workflow"),
			Entry("fails when listing remaining workflows during deletion", func() setupResult {
				testScheme := newBranchTestScheme()
				branch := newErrorTestBranch()
				now := metav1.Now()
				branch.DeletionTimestamp = &now
				baseClient := newBranchFakeClient(testScheme, branch)
				reconciler := &BranchReconciler{
					Client:              &workflowListSequenceClient{Client: baseClient, err: fmt.Errorf("list remaining failed")},
					Scheme:              testScheme,
					GitHubClientManager: &fakeGitHubClientManager{},
				}
				return setupResult{reconciler: reconciler, request: makeRequest(branch)}
			}, "list remaining failed"),
			Entry("fails when removing finalizer update errors", func() setupResult {
				testScheme := newBranchTestScheme()
				branch := newErrorTestBranch()
				now := metav1.Now()
				branch.DeletionTimestamp = &now
				baseClient := newBranchFakeClient(testScheme, branch)
				reconciler := &BranchReconciler{
					Client:              &updateErrorClient{Client: baseClient, err: fmt.Errorf("remove finalizer update failed")},
					Scheme:              testScheme,
					GitHubClientManager: &fakeGitHubClientManager{},
				}
				return setupResult{reconciler: reconciler, request: makeRequest(branch)}
			}, "remove finalizer update failed"),
			Entry("fails when deleting completed branch", func() setupResult {
				testScheme := newBranchTestScheme()
				branch := newErrorTestBranch()
				branch.UID = types.UID("branch-completed-delete")
				branch.Spec.Name = defaultBranchName
				repo := repoForBranch(branch)
				workflow := &terrakojoiov1alpha1.Workflow{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "workflow-completed-delete",
						Namespace: branch.Namespace,
					},
					Spec: terrakojoiov1alpha1.WorkflowSpec{
						Owner:      branch.Spec.Owner,
						Repository: branch.Spec.Repository,
						Branch:     branch.Name,
						SHA:        branch.Spec.SHA,
						Template:   "template",
					},
					Status: terrakojoiov1alpha1.WorkflowStatus{
						Phase: string(WorkflowPhaseSucceeded),
					},
				}
				Expect(controllerutil.SetControllerReference(branch, workflow, testScheme)).To(Succeed())
				baseClient := newBranchFakeClient(testScheme, branch, workflow, repo)
				reconciler := &BranchReconciler{
					Client:              &deleteErrorClient{Client: baseClient, err: fmt.Errorf("delete branch failed")},
					Scheme:              testScheme,
					GitHubClientManager: &fakeGitHubClientManager{},
				}
				return setupResult{reconciler: reconciler, request: makeRequest(branch)}
			}, "delete branch failed"),
			Entry("fails when deleting workflows after SHA change", func() setupResult {
				testScheme := newBranchTestScheme()
				branch := newErrorTestBranch()
				branch.Annotations = map[string]string{
					"terrakojo.io/last-sha": "0123456789abcdef0123456789abcdef01234567",
				}
				branch.Spec.SHA = "0123456789abcdef0123456789abcdef01234568"
				branch.UID = types.UID("branch-sha-change")
				repo := repoForBranch(branch)
				workflow := &terrakojoiov1alpha1.Workflow{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "workflow-sha-change",
						Namespace: branch.Namespace,
					},
					Spec: terrakojoiov1alpha1.WorkflowSpec{
						Owner:      branch.Spec.Owner,
						Repository: branch.Spec.Repository,
						Branch:     branch.Name,
						SHA:        "0123456789abcdef0123456789abcdef01234567",
						Template:   "template",
					},
				}
				Expect(controllerutil.SetControllerReference(branch, workflow, testScheme)).To(Succeed())
				baseClient := newBranchFakeClient(testScheme, branch, repo, workflow)
				reconciler := &BranchReconciler{
					Client:              &deleteWorkflowErrorClient{Client: baseClient, err: fmt.Errorf("delete workflow error")},
					Scheme:              testScheme,
					GitHubClientManager: &fakeGitHubClientManager{},
				}
				return setupResult{reconciler: reconciler, request: makeRequest(branch)}
			}, "failed to delete workflow"),
			Entry("fails when PR changed files returns error", func() setupResult {
				testScheme := newBranchTestScheme()
				branch := newErrorTestBranch()
				branch.Spec.PRNumber = 7
				repo := repoForBranch(branch)
				client := newBranchFakeClient(testScheme, branch, repo)
				ghManager := &fakeGitHubClientManager{
					GetClientForBranchFunc: func(ctx context.Context, branch *terrakojoiov1alpha1.Branch) (gh.ClientInterface, error) {
						return &fakeGitHubClient{
							GetChangedFilesFunc: func(owner, repo string, prNumber int) ([]string, error) {
								return nil, fmt.Errorf("pr changed files error")
							},
						}, nil
					},
				}
				reconciler := &BranchReconciler{
					Client:              client,
					Scheme:              testScheme,
					GitHubClientManager: ghManager,
				}
				return setupResult{reconciler: reconciler, request: makeRequest(branch)}
			}, "pr changed files error"),
			Entry("fails when deleting branch with no changed files", func() setupResult {
				testScheme := newBranchTestScheme()
				branch := newErrorTestBranch()
				branch.Spec.Name = defaultBranchName
				repo := repoForBranch(branch)
				baseClient := newBranchFakeClient(testScheme, branch, repo)
				ghManager := &fakeGitHubClientManager{
					GetClientForBranchFunc: func(ctx context.Context, branch *terrakojoiov1alpha1.Branch) (gh.ClientInterface, error) {
						return &fakeGitHubClient{
							GetChangedFilesForCommitFunc: func(owner, repo, sha string) ([]string, error) {
								return []string{}, nil
							},
						}, nil
					},
				}
				reconciler := &BranchReconciler{
					Client:              &deleteErrorClient{Client: baseClient, err: fmt.Errorf("delete branch failed")},
					Scheme:              testScheme,
					GitHubClientManager: ghManager,
				}
				return setupResult{reconciler: reconciler, request: makeRequest(branch)}
			}, "delete branch failed"),
			Entry("fails when deleting branch with no matching templates", func() setupResult {
				testScheme := newBranchTestScheme()
				branch := newErrorTestBranch()
				branch.Spec.Name = defaultBranchName
				repo := repoForBranch(branch)
				template := &terrakojoiov1alpha1.WorkflowTemplate{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "template-no-match-delete",
						Namespace: branch.Namespace,
					},
					Spec: terrakojoiov1alpha1.WorkflowTemplateSpec{
						DisplayName: "no-match",
						Match: terrakojoiov1alpha1.WorkflowMatch{
							Paths: []string{"infrastructure/**/*.tf"},
						},
						Job: newBranchTemplateJobSpec("plan", "hashicorp/terraform:latest", []string{"echo", "Planning..."}),
					},
				}
				baseClient := newBranchFakeClient(testScheme, branch, repo, template)
				ghManager := &fakeGitHubClientManager{
					GetClientForBranchFunc: func(ctx context.Context, branch *terrakojoiov1alpha1.Branch) (gh.ClientInterface, error) {
						return &fakeGitHubClient{
							GetChangedFilesForCommitFunc: func(owner, repo, sha string) ([]string, error) {
								return []string{"docs/readme.md"}, nil
							},
						}, nil
					},
				}
				reconciler := &BranchReconciler{
					Client:              &deleteErrorClient{Client: baseClient, err: fmt.Errorf("delete branch failed")},
					Scheme:              testScheme,
					GitHubClientManager: ghManager,
				}
				return setupResult{reconciler: reconciler, request: makeRequest(branch)}
			}, "delete branch failed"),
			Entry("fails when create workflow returns error", func() setupResult {
				testScheme := newBranchTestScheme()
				branch := newErrorTestBranch()
				repo := repoForBranch(branch)
				template := &terrakojoiov1alpha1.WorkflowTemplate{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "template-create-error",
						Namespace: branch.Namespace,
					},
					Spec: terrakojoiov1alpha1.WorkflowTemplateSpec{
						DisplayName: "create-error",
						Match: terrakojoiov1alpha1.WorkflowMatch{
							Paths: []string{"dir/*.tf"},
						},
						Job: newBranchTemplateJobSpec("plan", "hashicorp/terraform:latest", []string{"echo", "Planning..."}),
					},
				}
				baseClient := newBranchFakeClient(testScheme, branch, repo, template)
				ghManager := &fakeGitHubClientManager{
					GetClientForBranchFunc: func(ctx context.Context, branch *terrakojoiov1alpha1.Branch) (gh.ClientInterface, error) {
						return &fakeGitHubClient{
							GetChangedFilesForCommitFunc: func(owner, repo, sha string) ([]string, error) {
								return []string{"dir/file.tf"}, nil
							},
						}, nil
					},
				}
				reconciler := &BranchReconciler{
					Client:              &createWorkflowErrorClient{Client: baseClient, err: fmt.Errorf("create workflow error")},
					Scheme:              testScheme,
					GitHubClientManager: ghManager,
				}
				return setupResult{reconciler: reconciler, request: makeRequest(branch)}
			}, "create workflow error"),
			Entry("fails when updating branch annotations returns error", func() setupResult {
				testScheme := newBranchTestScheme()
				branch := newErrorTestBranch()
				repo := repoForBranch(branch)
				template := &terrakojoiov1alpha1.WorkflowTemplate{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "template-update-error",
						Namespace: branch.Namespace,
					},
					Spec: terrakojoiov1alpha1.WorkflowTemplateSpec{
						DisplayName: "update-error",
						Match: terrakojoiov1alpha1.WorkflowMatch{
							Paths: []string{"dir/*.tf"},
						},
						Job: newBranchTemplateJobSpec("plan", "hashicorp/terraform:latest", []string{"echo", "Planning..."}),
					},
				}
				baseClient := newBranchFakeClient(testScheme, branch, repo, template)
				ghManager := &fakeGitHubClientManager{
					GetClientForBranchFunc: func(ctx context.Context, branch *terrakojoiov1alpha1.Branch) (gh.ClientInterface, error) {
						return &fakeGitHubClient{
							GetChangedFilesForCommitFunc: func(owner, repo, sha string) ([]string, error) {
								return []string{"dir/file.tf"}, nil
							},
						}, nil
					},
				}
				reconciler := &BranchReconciler{
					Client:              &updateErrorClient{Client: baseClient, err: fmt.Errorf("update annotations failed")},
					Scheme:              testScheme,
					GitHubClientManager: ghManager,
				}
				return setupResult{reconciler: reconciler, request: makeRequest(branch)}
			}, "update annotations failed"),
			Entry("fails when updating branch status returns error", func() setupResult {
				testScheme := newBranchTestScheme()
				branch := newErrorTestBranch()
				repo := repoForBranch(branch)
				template := &terrakojoiov1alpha1.WorkflowTemplate{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "template-status-error",
						Namespace: branch.Namespace,
					},
					Spec: terrakojoiov1alpha1.WorkflowTemplateSpec{
						DisplayName: "status-error",
						Match: terrakojoiov1alpha1.WorkflowMatch{
							Paths: []string{"dir/*.tf"},
						},
						Job: newBranchTemplateJobSpec("plan", "hashicorp/terraform:latest", []string{"echo", "Planning..."}),
					},
				}
				baseClient := newBranchFakeClient(testScheme, branch, repo, template)
				ghManager := &fakeGitHubClientManager{
					GetClientForBranchFunc: func(ctx context.Context, branch *terrakojoiov1alpha1.Branch) (gh.ClientInterface, error) {
						return &fakeGitHubClient{
							GetChangedFilesForCommitFunc: func(owner, repo, sha string) ([]string, error) {
								return []string{"dir/file.tf"}, nil
							},
						}, nil
					},
				}
				reconciler := &BranchReconciler{
					Client:              &statusErrorClient{Client: baseClient, err: fmt.Errorf("status update failed")},
					Scheme:              testScheme,
					GitHubClientManager: ghManager,
				}
				return setupResult{reconciler: reconciler, request: makeRequest(branch)}
			}, "status update failed"),
		)
	})

	When("testing helper methods", func() {
		It("updates an existing condition of the same type", func() {
			reconciler := &BranchReconciler{}
			branch := &terrakojoiov1alpha1.Branch{
				Status: terrakojoiov1alpha1.BranchStatus{
					Conditions: []metav1.Condition{
						{
							Type:    "WorkflowReady",
							Status:  metav1.ConditionFalse,
							Reason:  "OldReason",
							Message: "old message",
						},
					},
				},
			}

			reconciler.setCondition(branch, "WorkflowReady", metav1.ConditionTrue, "NewReason", "new message")

			Expect(branch.Status.Conditions).To(HaveLen(1))
			Expect(branch.Status.Conditions[0].Status).To(Equal(metav1.ConditionTrue))
			Expect(branch.Status.Conditions[0].Reason).To(Equal("NewReason"))
			Expect(branch.Status.Conditions[0].Message).To(Equal("new message"))
			Expect(branch.Status.Conditions[0].LastTransitionTime.IsZero()).To(BeFalse())
		})

		It("returns nil when no owner reference matches", func() {
			controllerFalse := false
			workflow := &terrakojoiov1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{
							Kind:       "Branch",
							UID:        types.UID("branch-uid"),
							Controller: &controllerFalse,
						},
					},
				},
			}
			Expect(indexByOwnerBranchUID(workflow)).To(BeNil())
		})

		It("allWorkflowsCompleted returns false for empty or non-terminal workflows", func() {
			Expect(allWorkflowsCompleted(nil)).To(BeFalse())
			workflows := []terrakojoiov1alpha1.Workflow{
				{
					Status: terrakojoiov1alpha1.WorkflowStatus{
						Phase: string(WorkflowPhaseRunning),
					},
				},
			}
			Expect(allWorkflowsCompleted(workflows)).To(BeFalse())
		})

		It("deleteWorkflowsForBranch returns error when list fails", func() {
			testScheme := newBranchTestScheme()
			branch := newErrorTestBranch()
			reconciler := &BranchReconciler{
				Client: &listErrorClient{
					Client: newBranchFakeClient(testScheme, branch),
					err:    fmt.Errorf("list failed"),
				},
				Scheme: testScheme,
			}
			err := reconciler.deleteWorkflowsForBranch(context.Background(), branch)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to list workflows"))
		})

		It("createWorkflowForBranch returns error when workflow GVK is missing", func() {
			testScheme := runtime.NewScheme()
			testScheme.AddKnownTypes(terrakojoiov1alpha1.GroupVersion, &terrakojoiov1alpha1.Branch{}, &terrakojoiov1alpha1.BranchList{})
			metav1.AddToGroupVersion(testScheme, terrakojoiov1alpha1.GroupVersion)
			reconciler := &BranchReconciler{
				Client: fake.NewClientBuilder().WithScheme(testScheme).Build(),
				Scheme: testScheme,
			}
			branch := &terrakojoiov1alpha1.Branch{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "branch-missing-workflow-gvk",
					Namespace: "default",
					UID:       types.UID("branch-missing-workflow-gvk"),
				},
				Spec: terrakojoiov1alpha1.BranchSpec{
					Owner:      "owner",
					Repository: "repo",
					Name:       "feature/gvk",
					SHA:        "0123456789abcdef0123456789abcdef01234567",
				},
			}
			_, err := reconciler.createWorkflowForBranch(
				context.Background(),
				branch,
				"template",
				"workflow",
				"path",
				false,
				terrakojoiov1alpha1.WorkflowExecutionUnitFolder,
			)
			Expect(err).To(HaveOccurred())
		})

		It("createWorkflowForBranch sets runtime parameters", func() {
			testScheme := newBranchTestScheme()
			branch := &terrakojoiov1alpha1.Branch{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "branch-default-param",
					Namespace: "default",
					UID:       types.UID("branch-default-param"),
				},
				Spec: terrakojoiov1alpha1.BranchSpec{
					Owner:      "owner",
					Repository: "repo",
					Name:       "main",
					SHA:        "0123456789abcdef0123456789abcdef01234567",
				},
			}

			reconciler := &BranchReconciler{
				Client: newBranchFakeClient(testScheme, branch),
				Scheme: testScheme,
			}

			createdName, err := reconciler.createWorkflowForBranch(
				context.Background(),
				branch,
				"template",
				"workflow-",
				".",
				true,
				terrakojoiov1alpha1.WorkflowExecutionUnitRepository,
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(createdName).NotTo(BeEmpty())

			var workflows terrakojoiov1alpha1.WorkflowList
			Expect(reconciler.List(context.Background(), &workflows, client.InNamespace(branch.Namespace))).To(Succeed())
			Expect(workflows.Items).To(HaveLen(1))
			Expect(workflows.Items[0].Spec.Parameters).To(HaveKeyWithValue("isDefaultBranch", "true"))
			Expect(workflows.Items[0].Spec.Parameters).To(HaveKeyWithValue("executionUnit", "repository"))
		})

		It("workflowTargets falls back to folder semantics for invalid executionUnit", func() {
			targets := workflowTargets(
				terrakojoiov1alpha1.WorkflowExecutionUnit("weird"),
				[]string{
					"infrastructure/app/main.tf",
					"infrastructure/db/variables.tf",
					"infrastructure/db/variables.tf",
					"README.md",
				},
			)

			Expect(targets).To(HaveLen(2))
			Expect(targets[0].path).To(Equal("infrastructure/app"))
			Expect(targets[0].executionUnit).To(Equal(terrakojoiov1alpha1.WorkflowExecutionUnitFolder))
			Expect(targets[1].path).To(Equal("infrastructure/db"))
			Expect(targets[1].executionUnit).To(Equal(terrakojoiov1alpha1.WorkflowExecutionUnitFolder))
		})
	})
})
