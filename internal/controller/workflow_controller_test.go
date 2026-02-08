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
	"fmt"
	"strings"
	"sync/atomic"

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
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	ghapi "github.com/google/go-github/v79/github"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	terrakojoiov1alpha1 "github.com/eeekcct/terrakojo/api/v1alpha1"
	gh "github.com/eeekcct/terrakojo/internal/github"
)

func newWorkflowTestScheme() *runtime.Scheme {
	testScheme := runtime.NewScheme()
	Expect(terrakojoiov1alpha1.AddToScheme(testScheme)).To(Succeed())
	Expect(batchv1.AddToScheme(testScheme)).To(Succeed())
	Expect(corev1.AddToScheme(testScheme)).To(Succeed())
	return testScheme
}

func newWorkflowFakeClient(testScheme *runtime.Scheme, objs ...client.Object) client.Client {
	builder := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithStatusSubresource(&terrakojoiov1alpha1.Workflow{}, &terrakojoiov1alpha1.Branch{}, &terrakojoiov1alpha1.Repository{}, &batchv1.Job{})
	if len(objs) > 0 {
		builder.WithObjects(objs...)
	}
	return builder.Build()
}

func newTemplateJobSpec(containerName, image string, command []string) batchv1.JobSpec {
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

type branchGetErrorClient struct {
	client.Client
	err error
}

func (c *branchGetErrorClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*terrakojoiov1alpha1.Branch); ok {
		return c.err
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

type jobGetErrorClient struct {
	client.Client
	err error
}

func (c *jobGetErrorClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*batchv1.Job); ok {
		return c.err
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

type workflowDeleteErrorClient struct {
	client.Client
	err error
}

func (c *workflowDeleteErrorClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if _, ok := obj.(*terrakojoiov1alpha1.Workflow); ok {
		return c.err
	}
	return c.Client.Delete(ctx, obj, opts...)
}

type jobCreateErrorClient struct {
	client.Client
	err error
}

func (c *jobCreateErrorClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, ok := obj.(*batchv1.Job); ok {
		return c.err
	}
	return c.Client.Create(ctx, obj, opts...)
}

type workflowGetErrorClient struct {
	client.Client
	err error
}

func (c *workflowGetErrorClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*terrakojoiov1alpha1.Workflow); ok {
		return c.err
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

var _ = Describe("Workflow Controller", func() {
	When("updating workflow status (envtest)", func() {
		It("updates workflow status", func() {
			ctx := context.Background()
			namespace := createTestNamespace(ctx)
			workflow := &terrakojoiov1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{
					Name:      uniqueName("workflow-status"),
					Namespace: namespace,
				},
				Spec: terrakojoiov1alpha1.WorkflowSpec{
					Owner:      "test-owner",
					Repository: "test-repo",
					Branch:     "branch-ref",
					SHA:        "0123456789abcdef0123456789abcdef01234567",
					Template:   "test-template",
					Path:       "infra/path",
				},
			}
			Expect(k8sClient.Create(ctx, workflow)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, workflow)
			})

			reconciler := &WorkflowReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			err := reconciler.updateWorkflowStatus(ctx, workflow, WorkflowPhaseRunning)
			Expect(err).NotTo(HaveOccurred())

			updated := &terrakojoiov1alpha1.Workflow{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(workflow), updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(string(WorkflowPhaseRunning)))
			Expect(updated.Status.Conditions).NotTo(BeEmpty())
		})

		It("retries status updates on conflict", func() {
			ctx := context.Background()
			namespace := createTestNamespace(ctx)
			workflow := &terrakojoiov1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{
					Name:      uniqueName("workflow-conflict"),
					Namespace: namespace,
				},
				Spec: terrakojoiov1alpha1.WorkflowSpec{
					Owner:      "test-owner",
					Repository: "test-repo",
					Branch:     "branch-ref",
					SHA:        "0123456789abcdef0123456789abcdef01234567",
					Template:   "test-template",
					Path:       "infra/path",
				},
			}
			Expect(k8sClient.Create(ctx, workflow)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, workflow)
			})

			reconciler := &WorkflowReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			var attempts int32
			var conflictInjected int32
			err := reconciler.updateWorkflowStatusWithRetry(ctx, workflow, func(latest *terrakojoiov1alpha1.Workflow) {
				atomic.AddInt32(&attempts, 1)
				if atomic.CompareAndSwapInt32(&conflictInjected, 0, 1) {
					other := latest.DeepCopy()
					other.Status.CheckRunName = "conflict"
					Expect(reconciler.Status().Update(ctx, other)).To(Succeed())
				}
				latest.Status.Phase = string(WorkflowPhaseRunning)
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(attempts).To(BeNumerically(">=", 2))

			updated := &terrakojoiov1alpha1.Workflow{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(workflow), updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(string(WorkflowPhaseRunning)))
		})
	})

	When("reconciling Workflow resources (unit paths)", func() {
		It("ignores reconcile when workflow is missing", func() {
			ctx := context.Background()
			testScheme := newWorkflowTestScheme()

			reconciler := &WorkflowReconciler{
				Client: newWorkflowFakeClient(testScheme),
				Scheme: testScheme,
			}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "missing-workflow", Namespace: "default"}})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns nil when workflow template is missing", func() {
			ctx := context.Background()
			testScheme := newWorkflowTestScheme()

			workflow := &terrakojoiov1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "workflow-no-template",
					Namespace:  "default",
					Finalizers: []string{workflowFinalizer},
				},
				Spec: terrakojoiov1alpha1.WorkflowSpec{
					Owner:      "owner",
					Repository: "repo",
					Branch:     "branch-ref",
					SHA:        "0123456789abcdef0123456789abcdef01234567",
					Template:   "missing-template",
					Path:       "path",
				},
			}
			branch := &terrakojoiov1alpha1.Branch{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "branch-ref",
					Namespace: workflow.Namespace,
				},
				Spec: terrakojoiov1alpha1.BranchSpec{
					Owner:      workflow.Spec.Owner,
					Repository: workflow.Spec.Repository,
					Name:       "feature",
					SHA:        workflow.Spec.SHA,
				},
			}

			fakeClient := newWorkflowFakeClient(testScheme, workflow, branch)
			ghManager := &fakeGitHubClientManager{
				GetClientForBranchFunc: func(ctx context.Context, branch *terrakojoiov1alpha1.Branch) (gh.ClientInterface, error) {
					return &fakeGitHubClient{}, nil
				},
			}
			reconciler := &WorkflowReconciler{
				Client:              fakeClient,
				Scheme:              testScheme,
				GitHubClientManager: ghManager,
			}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workflow)})
			Expect(err).NotTo(HaveOccurred())

			job := &batchv1.Job{}
			err = fakeClient.Get(ctx, client.ObjectKey{Name: "workflow-no-template", Namespace: workflow.Namespace}, job)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("adds finalizer when missing", func() {
			ctx := context.Background()
			testScheme := newWorkflowTestScheme()

			workflow := &terrakojoiov1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "workflow-finalizer",
					Namespace: "default",
				},
				Spec: terrakojoiov1alpha1.WorkflowSpec{
					Owner:      "owner",
					Repository: "repo",
					Branch:     "branch-ref",
					SHA:        "0123456789abcdef0123456789abcdef01234567",
					Template:   "template",
					Path:       "path",
				},
			}
			branch := &terrakojoiov1alpha1.Branch{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "branch-ref",
					Namespace: workflow.Namespace,
				},
				Spec: terrakojoiov1alpha1.BranchSpec{
					Owner:      workflow.Spec.Owner,
					Repository: workflow.Spec.Repository,
					Name:       "feature",
					SHA:        workflow.Spec.SHA,
				},
			}

			fakeClient := newWorkflowFakeClient(testScheme, workflow, branch)
			ghManager := &fakeGitHubClientManager{
				GetClientForBranchFunc: func(ctx context.Context, branch *terrakojoiov1alpha1.Branch) (gh.ClientInterface, error) {
					return &fakeGitHubClient{}, nil
				},
			}
			reconciler := &WorkflowReconciler{
				Client:              fakeClient,
				Scheme:              testScheme,
				GitHubClientManager: ghManager,
			}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workflow)})
			Expect(err).NotTo(HaveOccurred())

			updated := &terrakojoiov1alpha1.Workflow{}
			Expect(fakeClient.Get(ctx, client.ObjectKeyFromObject(workflow), updated)).To(Succeed())
			Expect(updated.Finalizers).To(ContainElement(workflowFinalizer))
		})

		It("deletes workflow when branch is missing", func() {
			ctx := context.Background()
			testScheme := newWorkflowTestScheme()

			workflow := &terrakojoiov1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "workflow-branch-missing",
					Namespace:  "default",
					Finalizers: []string{workflowFinalizer},
				},
				Spec: terrakojoiov1alpha1.WorkflowSpec{
					Owner:      "owner",
					Repository: "repo",
					Branch:     "branch-ref",
					SHA:        "0123456789abcdef0123456789abcdef01234567",
					Template:   "template",
					Path:       "path",
				},
			}

			fakeClient := newWorkflowFakeClient(testScheme, workflow)
			reconciler := &WorkflowReconciler{
				Client:              fakeClient,
				Scheme:              testScheme,
				GitHubClientManager: &fakeGitHubClientManager{},
			}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workflow)})
			Expect(err).NotTo(HaveOccurred())

			fetched := &terrakojoiov1alpha1.Workflow{}
			err = fakeClient.Get(ctx, client.ObjectKeyFromObject(workflow), fetched)
			if err != nil {
				Expect(apierrors.IsNotFound(err)).To(BeTrue())
				return
			}
			Expect(fetched.DeletionTimestamp.IsZero()).To(BeFalse())
		})

		It("logs error when deleting workflow for missing branch fails", func() {
			ctx := context.Background()
			testScheme := newWorkflowTestScheme()

			workflow := &terrakojoiov1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "workflow-branch-missing-delete-error",
					Namespace:  "default",
					Finalizers: []string{workflowFinalizer},
				},
				Spec: terrakojoiov1alpha1.WorkflowSpec{
					Owner:      "owner",
					Repository: "repo",
					Branch:     "branch-ref",
					SHA:        "0123456789abcdef0123456789abcdef01234567",
					Template:   "template",
					Path:       "path",
				},
			}

			baseClient := newWorkflowFakeClient(testScheme, workflow)
			reconciler := &WorkflowReconciler{
				Client:              &workflowDeleteErrorClient{Client: baseClient, err: fmt.Errorf("delete error")},
				Scheme:              testScheme,
				GitHubClientManager: &fakeGitHubClientManager{},
			}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workflow)})
			Expect(err).NotTo(HaveOccurred())

			fetched := &terrakojoiov1alpha1.Workflow{}
			Expect(baseClient.Get(ctx, client.ObjectKeyFromObject(workflow), fetched)).To(Succeed())
		})

		It("creates job and updates status when job is missing", func() {
			ctx := context.Background()
			testScheme := newWorkflowTestScheme()

			workflow := &terrakojoiov1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "workflow-create-job",
					Namespace:  "default",
					Finalizers: []string{workflowFinalizer},
				},
				Spec: terrakojoiov1alpha1.WorkflowSpec{
					Owner:      "owner",
					Repository: "repo",
					Branch:     "branch-ref",
					SHA:        "0123456789abcdef0123456789abcdef01234567",
					Template:   "template",
					Path:       "infra/path",
				},
			}
			branch := &terrakojoiov1alpha1.Branch{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "branch-ref",
					Namespace: workflow.Namespace,
				},
				Spec: terrakojoiov1alpha1.BranchSpec{
					Owner:      workflow.Spec.Owner,
					Repository: workflow.Spec.Repository,
					Name:       "feature",
					SHA:        workflow.Spec.SHA,
				},
			}
			template := &terrakojoiov1alpha1.WorkflowTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      workflow.Spec.Template,
					Namespace: workflow.Namespace,
				},
				Spec: terrakojoiov1alpha1.WorkflowTemplateSpec{
					DisplayName: "Test Workflow",
					Match: terrakojoiov1alpha1.WorkflowMatch{
						Paths: []string{"**/*"},
					},
					Job: newTemplateJobSpec("Plan Step", "busybox", []string{"echo", "hello"}),
				},
			}

			fakeClient := newWorkflowFakeClient(testScheme, workflow, branch, template)
			var updateCalled atomic.Bool
			checkRunID := int64(123)
			ghManager := &fakeGitHubClientManager{
				GetClientForBranchFunc: func(ctx context.Context, branch *terrakojoiov1alpha1.Branch) (gh.ClientInterface, error) {
					return &fakeGitHubClient{
						CreateCheckRunFunc: func(owner, repo, sha, name string) (*ghapi.CheckRun, error) {
							return &ghapi.CheckRun{ID: &checkRunID}, nil
						},
						UpdateCheckRunFunc: func(owner, repo string, checkRunID int64, name, status, conclusion string) error {
							updateCalled.Store(true)
							return nil
						},
					}, nil
				},
			}
			reconciler := &WorkflowReconciler{
				Client:              fakeClient,
				Scheme:              testScheme,
				GitHubClientManager: ghManager,
			}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workflow)})
			Expect(err).NotTo(HaveOccurred())

			job := &batchv1.Job{}
			Expect(fakeClient.Get(ctx, client.ObjectKey{Name: "workflow-create-job", Namespace: workflow.Namespace}, job)).To(Succeed())
			Expect(job.Spec.Template.Spec.Containers[0].Name).To(Equal("plan-step"))
			Expect(job.Spec.BackoffLimit).NotTo(BeNil())
			Expect(*job.Spec.BackoffLimit).To(Equal(int32(0)))

			updated := &terrakojoiov1alpha1.Workflow{}
			Expect(fakeClient.Get(ctx, client.ObjectKeyFromObject(workflow), updated)).To(Succeed())
			Expect(updated.Status.CheckRunID).To(Equal(int(checkRunID)))
			Expect(updated.Status.CheckRunName).To(Equal("Test Workflow(infra/path)"))
			Expect(updated.Status.Phase).To(Equal(string(WorkflowPhasePending)))
			Expect(updateCalled.Load()).To(BeTrue())
		})

		It("updates workflow when job exists", func() {
			ctx := context.Background()
			testScheme := newWorkflowTestScheme()

			workflow := &terrakojoiov1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "workflow-job-exists",
					Namespace:  "default",
					Finalizers: []string{workflowFinalizer},
				},
				Spec: terrakojoiov1alpha1.WorkflowSpec{
					Owner:      "owner",
					Repository: "repo",
					Branch:     "branch-ref",
					SHA:        "0123456789abcdef0123456789abcdef01234567",
					Template:   "template",
					Path:       "infra/path",
				},
				Status: terrakojoiov1alpha1.WorkflowStatus{
					CheckRunName: "check",
					CheckRunID:   11,
				},
			}
			branch := &terrakojoiov1alpha1.Branch{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "branch-ref",
					Namespace: workflow.Namespace,
				},
				Spec: terrakojoiov1alpha1.BranchSpec{
					Owner:      workflow.Spec.Owner,
					Repository: workflow.Spec.Repository,
					Name:       "feature",
					SHA:        workflow.Spec.SHA,
				},
			}
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "workflow-job-exists",
					Namespace: workflow.Namespace,
				},
				Status: batchv1.JobStatus{
					Succeeded: 1,
				},
			}
			fakeClient := newWorkflowFakeClient(testScheme, workflow, branch, job)
			var updateCalled atomic.Bool
			ghManager := &fakeGitHubClientManager{
				GetClientForBranchFunc: func(ctx context.Context, branch *terrakojoiov1alpha1.Branch) (gh.ClientInterface, error) {
					return &fakeGitHubClient{
						UpdateCheckRunFunc: func(owner, repo string, checkRunID int64, name, status, conclusion string) error {
							updateCalled.Store(true)
							return nil
						},
					}, nil
				},
			}
			reconciler := &WorkflowReconciler{
				Client:              fakeClient,
				Scheme:              testScheme,
				GitHubClientManager: ghManager,
			}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workflow)})
			Expect(err).NotTo(HaveOccurred())
			Expect(updateCalled.Load()).To(BeTrue())

			updated := &terrakojoiov1alpha1.Workflow{}
			Expect(fakeClient.Get(ctx, client.ObjectKeyFromObject(workflow), updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(string(WorkflowPhaseSucceeded)))
		})

		It("removes finalizer on deletion after cancelling checkrun", func() {
			ctx := context.Background()
			testScheme := newWorkflowTestScheme()
			now := metav1.Now()

			workflow := &terrakojoiov1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "workflow-delete",
					Namespace:         "default",
					Finalizers:        []string{workflowFinalizer},
					DeletionTimestamp: &now,
				},
				Spec: terrakojoiov1alpha1.WorkflowSpec{
					Owner:      "owner",
					Repository: "repo",
					Branch:     "branch-ref",
					SHA:        "0123456789abcdef0123456789abcdef01234567",
					Template:   "template",
					Path:       "infra/path",
				},
				Status: terrakojoiov1alpha1.WorkflowStatus{
					CheckRunName: "check",
					CheckRunID:   99,
				},
			}
			branch := &terrakojoiov1alpha1.Branch{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "branch-ref",
					Namespace: workflow.Namespace,
				},
				Spec: terrakojoiov1alpha1.BranchSpec{
					Owner:      workflow.Spec.Owner,
					Repository: workflow.Spec.Repository,
					Name:       "feature",
					SHA:        workflow.Spec.SHA,
				},
			}
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "workflow-delete",
					Namespace: workflow.Namespace,
				},
				Status: batchv1.JobStatus{
					Active: 1,
				},
			}
			fakeClient := newWorkflowFakeClient(testScheme, workflow, branch, job)
			var updateCalled atomic.Bool
			ghManager := &fakeGitHubClientManager{
				GetClientForBranchFunc: func(ctx context.Context, branch *terrakojoiov1alpha1.Branch) (gh.ClientInterface, error) {
					return &fakeGitHubClient{
						UpdateCheckRunFunc: func(owner, repo string, checkRunID int64, name, status, conclusion string) error {
							updateCalled.Store(true)
							return nil
						},
					}, nil
				},
			}
			reconciler := &WorkflowReconciler{
				Client:              fakeClient,
				Scheme:              testScheme,
				GitHubClientManager: ghManager,
			}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workflow)})
			Expect(err).NotTo(HaveOccurred())
			Expect(updateCalled.Load()).To(BeTrue())

			updated := &terrakojoiov1alpha1.Workflow{}
			err = fakeClient.Get(ctx, client.ObjectKeyFromObject(workflow), updated)
			if err != nil {
				Expect(apierrors.IsNotFound(err)).To(BeTrue())
				return
			}
			Expect(updated.Finalizers).NotTo(ContainElement(workflowFinalizer))
		})

		It("ignores not found error from updateWorkflowStatusWithRetry after checkrun creation", func() {
			ctx := context.Background()
			testScheme := newWorkflowTestScheme()

			workflow := &terrakojoiov1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "workflow-status-notfound",
					Namespace:  "default",
					Finalizers: []string{workflowFinalizer},
				},
				Spec: terrakojoiov1alpha1.WorkflowSpec{
					Owner:      "owner",
					Repository: "repo",
					Branch:     "branch-ref",
					SHA:        "0123456789abcdef0123456789abcdef01234567",
					Template:   "template",
					Path:       "path",
				},
			}
			branch := &terrakojoiov1alpha1.Branch{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "branch-ref",
					Namespace: workflow.Namespace,
				},
				Spec: terrakojoiov1alpha1.BranchSpec{
					Owner:      workflow.Spec.Owner,
					Repository: workflow.Spec.Repository,
					Name:       "feature",
					SHA:        workflow.Spec.SHA,
				},
			}
			template := &terrakojoiov1alpha1.WorkflowTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      workflow.Spec.Template,
					Namespace: workflow.Namespace,
				},
				Spec: terrakojoiov1alpha1.WorkflowTemplateSpec{
					DisplayName: "Test",
					Match:       terrakojoiov1alpha1.WorkflowMatch{Paths: []string{"**/*"}},
					Job:         newTemplateJobSpec("step", "busybox", []string{"echo"}),
				},
			}

			baseClient := newWorkflowFakeClient(testScheme, workflow, branch, template)
			notFoundErr := apierrors.NewNotFound(
				terrakojoiov1alpha1.GroupVersion.WithResource("workflows").GroupResource(),
				workflow.Name,
			)
			var createCalled atomic.Bool
			checkRunID := int64(1)
			ghManager := &fakeGitHubClientManager{
				GetClientForBranchFunc: func(ctx context.Context, branch *terrakojoiov1alpha1.Branch) (gh.ClientInterface, error) {
					return &fakeGitHubClient{
						CreateCheckRunFunc: func(owner, repo, sha, name string) (*ghapi.CheckRun, error) {
							createCalled.Store(true)
							return &ghapi.CheckRun{ID: &checkRunID}, nil
						},
					}, nil
				},
			}

			reconciler := &WorkflowReconciler{
				Client:              &statusErrorClient{Client: baseClient, err: notFoundErr},
				Scheme:              testScheme,
				GitHubClientManager: ghManager,
			}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workflow)})
			Expect(err).NotTo(HaveOccurred())
			Expect(createCalled.Load()).To(BeTrue())
		})
	})

	When("reconciling Workflow resources (error paths)", func() {
		type setupResult struct {
			reconciler *WorkflowReconciler
			request    ctrl.Request
		}

		makeRequest := func(workflow *terrakojoiov1alpha1.Workflow) ctrl.Request {
			return ctrl.Request{NamespacedName: types.NamespacedName{Name: workflow.Name, Namespace: workflow.Namespace}}
		}

		DescribeTable("returns an error",
			func(setup func() setupResult, expectedSubstring string) {
				result := setup()
				_, err := result.reconciler.Reconcile(context.Background(), result.request)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring(expectedSubstring))
			},
			Entry("fails when branch get errors", func() setupResult {
				testScheme := newWorkflowTestScheme()
				workflow := &terrakojoiov1alpha1.Workflow{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "workflow-branch-error",
						Namespace: "default",
					},
					Spec: terrakojoiov1alpha1.WorkflowSpec{
						Owner:      "owner",
						Repository: "repo",
						Branch:     "branch-ref",
						SHA:        "0123456789abcdef0123456789abcdef01234567",
						Template:   "template",
						Path:       "path",
					},
				}
				baseClient := newWorkflowFakeClient(testScheme, workflow)
				reconciler := &WorkflowReconciler{
					Client:              &branchGetErrorClient{Client: baseClient, err: fmt.Errorf("branch get error")},
					Scheme:              testScheme,
					GitHubClientManager: &fakeGitHubClientManager{},
				}
				return setupResult{reconciler: reconciler, request: makeRequest(workflow)}
			}, "branch get error"),
			Entry("fails when GitHubClientManager is nil", func() setupResult {
				testScheme := newWorkflowTestScheme()
				workflow := &terrakojoiov1alpha1.Workflow{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "workflow-nil-gh",
						Namespace: "default",
						Finalizers: []string{
							workflowFinalizer,
						},
					},
					Spec: terrakojoiov1alpha1.WorkflowSpec{
						Owner:      "owner",
						Repository: "repo",
						Branch:     "branch-ref",
						SHA:        "0123456789abcdef0123456789abcdef01234567",
						Template:   "template",
						Path:       "path",
					},
				}
				branch := &terrakojoiov1alpha1.Branch{
					ObjectMeta: metav1.ObjectMeta{Name: "branch-ref", Namespace: workflow.Namespace},
					Spec: terrakojoiov1alpha1.BranchSpec{
						Owner:      workflow.Spec.Owner,
						Repository: workflow.Spec.Repository,
						Name:       "feature",
						SHA:        workflow.Spec.SHA,
					},
				}
				client := newWorkflowFakeClient(testScheme, workflow, branch)
				reconciler := &WorkflowReconciler{Client: client, Scheme: testScheme}
				return setupResult{reconciler: reconciler, request: makeRequest(workflow)}
			}, "GitHubClientManager not initialized"),
			Entry("fails when GetClientForBranch returns error", func() setupResult {
				testScheme := newWorkflowTestScheme()
				workflow := &terrakojoiov1alpha1.Workflow{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "workflow-gh-error",
						Namespace: "default",
						Finalizers: []string{
							workflowFinalizer,
						},
					},
					Spec: terrakojoiov1alpha1.WorkflowSpec{
						Owner:      "owner",
						Repository: "repo",
						Branch:     "branch-ref",
						SHA:        "0123456789abcdef0123456789abcdef01234567",
						Template:   "template",
						Path:       "path",
					},
				}
				branch := &terrakojoiov1alpha1.Branch{
					ObjectMeta: metav1.ObjectMeta{Name: "branch-ref", Namespace: workflow.Namespace},
					Spec: terrakojoiov1alpha1.BranchSpec{
						Owner:      workflow.Spec.Owner,
						Repository: workflow.Spec.Repository,
						Name:       "feature",
						SHA:        workflow.Spec.SHA,
					},
				}
				client := newWorkflowFakeClient(testScheme, workflow, branch)
				ghManager := &fakeGitHubClientManager{
					GetClientForBranchFunc: func(ctx context.Context, branch *terrakojoiov1alpha1.Branch) (gh.ClientInterface, error) {
						return nil, fmt.Errorf("gh client error")
					},
				}
				reconciler := &WorkflowReconciler{Client: client, Scheme: testScheme, GitHubClientManager: ghManager}
				return setupResult{reconciler: reconciler, request: makeRequest(workflow)}
			}, "gh client error"),
			Entry("fails when handleWorkflowDeletion returns error", func() setupResult {
				testScheme := newWorkflowTestScheme()
				now := metav1.Now()
				workflow := &terrakojoiov1alpha1.Workflow{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "workflow-delete-error",
						Namespace:         "default",
						Finalizers:        []string{workflowFinalizer},
						DeletionTimestamp: &now,
					},
					Spec: terrakojoiov1alpha1.WorkflowSpec{
						Owner:      "owner",
						Repository: "repo",
						Branch:     "branch-ref",
						SHA:        "0123456789abcdef0123456789abcdef01234567",
						Template:   "template",
						Path:       "path",
					},
					Status: terrakojoiov1alpha1.WorkflowStatus{
						CheckRunName: "check",
						CheckRunID:   99,
					},
				}
				branch := &terrakojoiov1alpha1.Branch{
					ObjectMeta: metav1.ObjectMeta{Name: "branch-ref", Namespace: workflow.Namespace},
					Spec: terrakojoiov1alpha1.BranchSpec{
						Owner:      workflow.Spec.Owner,
						Repository: workflow.Spec.Repository,
						Name:       "feature",
						SHA:        workflow.Spec.SHA,
					},
				}
				job := &batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{Name: "workflow-delete-error", Namespace: workflow.Namespace},
					Status:     batchv1.JobStatus{Active: 1},
				}
				client := newWorkflowFakeClient(testScheme, workflow, branch, job)
				ghManager := &fakeGitHubClientManager{
					GetClientForBranchFunc: func(ctx context.Context, branch *terrakojoiov1alpha1.Branch) (gh.ClientInterface, error) {
						return &fakeGitHubClient{
							UpdateCheckRunFunc: func(owner, repo string, checkRunID int64, name, status, conclusion string) error {
								return fmt.Errorf("checkrun update error")
							},
						}, nil
					},
				}
				reconciler := &WorkflowReconciler{Client: client, Scheme: testScheme, GitHubClientManager: ghManager}
				return setupResult{reconciler: reconciler, request: makeRequest(workflow)}
			}, "checkrun update error"),
			Entry("fails when removing finalizer update errors", func() setupResult {
				testScheme := newWorkflowTestScheme()
				now := metav1.Now()
				workflow := &terrakojoiov1alpha1.Workflow{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "workflow-remove-finalizer-error",
						Namespace:         "default",
						Finalizers:        []string{workflowFinalizer},
						DeletionTimestamp: &now,
					},
					Spec: terrakojoiov1alpha1.WorkflowSpec{
						Owner:      "owner",
						Repository: "repo",
						Branch:     "branch-ref",
						SHA:        "0123456789abcdef0123456789abcdef01234567",
						Template:   "template",
						Path:       "path",
					},
				}
				client := newWorkflowFakeClient(testScheme, workflow)
				reconciler := &WorkflowReconciler{
					Client:              &updateErrorClient{Client: client, err: fmt.Errorf("remove finalizer update failed")},
					Scheme:              testScheme,
					GitHubClientManager: &fakeGitHubClientManager{},
				}
				return setupResult{reconciler: reconciler, request: makeRequest(workflow)}
			}, "remove finalizer update failed"),
			Entry("fails when job get returns error", func() setupResult {
				testScheme := newWorkflowTestScheme()
				workflow := &terrakojoiov1alpha1.Workflow{
					ObjectMeta: metav1.ObjectMeta{
						Name:       "workflow-job-get-error",
						Namespace:  "default",
						Finalizers: []string{workflowFinalizer},
					},
					Spec: terrakojoiov1alpha1.WorkflowSpec{
						Owner:      "owner",
						Repository: "repo",
						Branch:     "branch-ref",
						SHA:        "0123456789abcdef0123456789abcdef01234567",
						Template:   "template",
						Path:       "path",
					},
				}
				branch := &terrakojoiov1alpha1.Branch{
					ObjectMeta: metav1.ObjectMeta{Name: "branch-ref", Namespace: workflow.Namespace},
					Spec: terrakojoiov1alpha1.BranchSpec{
						Owner:      workflow.Spec.Owner,
						Repository: workflow.Spec.Repository,
						Name:       "feature",
						SHA:        workflow.Spec.SHA,
					},
				}
				baseClient := newWorkflowFakeClient(testScheme, workflow, branch)
				reconciler := &WorkflowReconciler{
					Client:              &jobGetErrorClient{Client: baseClient, err: fmt.Errorf("job get error")},
					Scheme:              testScheme,
					GitHubClientManager: &fakeGitHubClientManager{},
				}
				return setupResult{reconciler: reconciler, request: makeRequest(workflow)}
			}, "job get error"),
			Entry("fails when UpdateCheckRun errors with existing job", func() setupResult {
				testScheme := newWorkflowTestScheme()
				workflow := &terrakojoiov1alpha1.Workflow{
					ObjectMeta: metav1.ObjectMeta{
						Name:       "workflow-update-checkrun-error",
						Namespace:  "default",
						Finalizers: []string{workflowFinalizer},
					},
					Spec: terrakojoiov1alpha1.WorkflowSpec{
						Owner:      "owner",
						Repository: "repo",
						Branch:     "branch-ref",
						SHA:        "0123456789abcdef0123456789abcdef01234567",
						Template:   "template",
						Path:       "path",
					},
					Status: terrakojoiov1alpha1.WorkflowStatus{
						CheckRunName: "check",
						CheckRunID:   42,
					},
				}
				branch := &terrakojoiov1alpha1.Branch{
					ObjectMeta: metav1.ObjectMeta{Name: "branch-ref", Namespace: workflow.Namespace},
					Spec: terrakojoiov1alpha1.BranchSpec{
						Owner:      workflow.Spec.Owner,
						Repository: workflow.Spec.Repository,
						Name:       "feature",
						SHA:        workflow.Spec.SHA,
					},
				}
				job := &batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{Name: "workflow-update-checkrun-error", Namespace: workflow.Namespace},
					Status:     batchv1.JobStatus{Succeeded: 1},
				}
				client := newWorkflowFakeClient(testScheme, workflow, branch, job)
				ghManager := &fakeGitHubClientManager{
					GetClientForBranchFunc: func(ctx context.Context, branch *terrakojoiov1alpha1.Branch) (gh.ClientInterface, error) {
						return &fakeGitHubClient{
							UpdateCheckRunFunc: func(owner, repo string, checkRunID int64, name, status, conclusion string) error {
								return fmt.Errorf("update checkrun error")
							},
						}, nil
					},
				}
				reconciler := &WorkflowReconciler{Client: client, Scheme: testScheme, GitHubClientManager: ghManager}
				return setupResult{reconciler: reconciler, request: makeRequest(workflow)}
			}, "update checkrun error"),
			Entry("fails when updateWorkflowStatus errors with existing job", func() setupResult {
				testScheme := newWorkflowTestScheme()
				workflow := &terrakojoiov1alpha1.Workflow{
					ObjectMeta: metav1.ObjectMeta{
						Name:       "workflow-update-status-error",
						Namespace:  "default",
						Finalizers: []string{workflowFinalizer},
					},
					Spec: terrakojoiov1alpha1.WorkflowSpec{
						Owner:      "owner",
						Repository: "repo",
						Branch:     "branch-ref",
						SHA:        "0123456789abcdef0123456789abcdef01234567",
						Template:   "template",
						Path:       "path",
					},
					Status: terrakojoiov1alpha1.WorkflowStatus{
						CheckRunName: "check",
						CheckRunID:   42,
					},
				}
				branch := &terrakojoiov1alpha1.Branch{
					ObjectMeta: metav1.ObjectMeta{Name: "branch-ref", Namespace: workflow.Namespace},
					Spec: terrakojoiov1alpha1.BranchSpec{
						Owner:      workflow.Spec.Owner,
						Repository: workflow.Spec.Repository,
						Name:       "feature",
						SHA:        workflow.Spec.SHA,
					},
				}
				job := &batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{Name: "workflow-update-status-error", Namespace: workflow.Namespace},
					Status:     batchv1.JobStatus{Succeeded: 1},
				}
				baseClient := newWorkflowFakeClient(testScheme, workflow, branch, job)
				ghManager := &fakeGitHubClientManager{
					GetClientForBranchFunc: func(ctx context.Context, branch *terrakojoiov1alpha1.Branch) (gh.ClientInterface, error) {
						return &fakeGitHubClient{UpdateCheckRunFunc: func(owner, repo string, checkRunID int64, name, status, conclusion string) error {
							return nil
						}}, nil
					},
				}
				reconciler := &WorkflowReconciler{Client: &statusErrorClient{Client: baseClient, err: fmt.Errorf("status update failed")}, Scheme: testScheme, GitHubClientManager: ghManager}
				return setupResult{reconciler: reconciler, request: makeRequest(workflow)}
			}, "status update failed"),
			Entry("fails when CreateCheckRun errors", func() setupResult {
				testScheme := newWorkflowTestScheme()
				workflow := &terrakojoiov1alpha1.Workflow{
					ObjectMeta: metav1.ObjectMeta{
						Name:       "workflow-create-checkrun-error",
						Namespace:  "default",
						Finalizers: []string{workflowFinalizer},
					},
					Spec: terrakojoiov1alpha1.WorkflowSpec{
						Owner:      "owner",
						Repository: "repo",
						Branch:     "branch-ref",
						SHA:        "0123456789abcdef0123456789abcdef01234567",
						Template:   "template",
						Path:       "path",
					},
				}
				branch := &terrakojoiov1alpha1.Branch{
					ObjectMeta: metav1.ObjectMeta{Name: "branch-ref", Namespace: workflow.Namespace},
					Spec: terrakojoiov1alpha1.BranchSpec{
						Owner:      workflow.Spec.Owner,
						Repository: workflow.Spec.Repository,
						Name:       "feature",
						SHA:        workflow.Spec.SHA,
					},
				}
				template := &terrakojoiov1alpha1.WorkflowTemplate{
					ObjectMeta: metav1.ObjectMeta{Name: workflow.Spec.Template, Namespace: workflow.Namespace},
					Spec: terrakojoiov1alpha1.WorkflowTemplateSpec{
						DisplayName: "Test",
						Match:       terrakojoiov1alpha1.WorkflowMatch{Paths: []string{"**/*"}},
						Job:         newTemplateJobSpec("step", "busybox", []string{"echo"}),
					},
				}
				client := fake.NewClientBuilder().
					WithScheme(testScheme).
					WithStatusSubresource(&terrakojoiov1alpha1.Workflow{}).
					WithObjects(workflow, branch, template).
					Build()
				ghManager := &fakeGitHubClientManager{
					GetClientForBranchFunc: func(ctx context.Context, branch *terrakojoiov1alpha1.Branch) (gh.ClientInterface, error) {
						return &fakeGitHubClient{CreateCheckRunFunc: func(owner, repo, sha, name string) (*ghapi.CheckRun, error) {
							return nil, fmt.Errorf("create checkrun error")
						}}, nil
					},
				}
				reconciler := &WorkflowReconciler{Client: client, Scheme: testScheme, GitHubClientManager: ghManager}
				return setupResult{reconciler: reconciler, request: makeRequest(workflow)}
			}, "create checkrun error"),
			Entry("fails when updating workflow status after checkrun creation", func() setupResult {
				testScheme := newWorkflowTestScheme()
				workflow := &terrakojoiov1alpha1.Workflow{
					ObjectMeta: metav1.ObjectMeta{
						Name:       "workflow-checkrun-status-error",
						Namespace:  "default",
						Finalizers: []string{workflowFinalizer},
					},
					Spec: terrakojoiov1alpha1.WorkflowSpec{
						Owner:      "owner",
						Repository: "repo",
						Branch:     "branch-ref",
						SHA:        "0123456789abcdef0123456789abcdef01234567",
						Template:   "template",
						Path:       "path",
					},
				}
				branch := &terrakojoiov1alpha1.Branch{
					ObjectMeta: metav1.ObjectMeta{Name: "branch-ref", Namespace: workflow.Namespace},
					Spec: terrakojoiov1alpha1.BranchSpec{
						Owner:      workflow.Spec.Owner,
						Repository: workflow.Spec.Repository,
						Name:       "feature",
						SHA:        workflow.Spec.SHA,
					},
				}
				template := &terrakojoiov1alpha1.WorkflowTemplate{
					ObjectMeta: metav1.ObjectMeta{Name: workflow.Spec.Template, Namespace: workflow.Namespace},
					Spec: terrakojoiov1alpha1.WorkflowTemplateSpec{
						DisplayName: "Test",
						Match:       terrakojoiov1alpha1.WorkflowMatch{Paths: []string{"**/*"}},
						Job:         newTemplateJobSpec("step", "busybox", []string{"echo"}),
					},
				}
				baseClient := newWorkflowFakeClient(testScheme, workflow, branch, template)
				checkRunID := int64(1)
				ghManager := &fakeGitHubClientManager{
					GetClientForBranchFunc: func(ctx context.Context, branch *terrakojoiov1alpha1.Branch) (gh.ClientInterface, error) {
						return &fakeGitHubClient{CreateCheckRunFunc: func(owner, repo, sha, name string) (*ghapi.CheckRun, error) {
							return &ghapi.CheckRun{ID: &checkRunID}, nil
						}}, nil
					},
				}
				reconciler := &WorkflowReconciler{Client: &statusErrorClient{Client: baseClient, err: fmt.Errorf("status update failed")}, Scheme: testScheme, GitHubClientManager: ghManager}
				return setupResult{reconciler: reconciler, request: makeRequest(workflow)}
			}, "status update failed"),
			Entry("fails when SetControllerReference errors", func() setupResult {
				testScheme := runtime.NewScheme()
				Expect(terrakojoiov1alpha1.AddToScheme(testScheme)).To(Succeed())
				workflow := &terrakojoiov1alpha1.Workflow{
					ObjectMeta: metav1.ObjectMeta{
						Name:       "workflow-setref-error",
						Namespace:  "default",
						Finalizers: []string{workflowFinalizer},
					},
					Spec: terrakojoiov1alpha1.WorkflowSpec{
						Owner:      "owner",
						Repository: "repo",
						Branch:     "branch-ref",
						SHA:        "0123456789abcdef0123456789abcdef01234567",
						Template:   "template",
						Path:       "path",
					},
				}
				branch := &terrakojoiov1alpha1.Branch{
					ObjectMeta: metav1.ObjectMeta{Name: "branch-ref", Namespace: workflow.Namespace},
					Spec: terrakojoiov1alpha1.BranchSpec{
						Owner:      workflow.Spec.Owner,
						Repository: workflow.Spec.Repository,
						Name:       "feature",
						SHA:        workflow.Spec.SHA,
					},
				}
				template := &terrakojoiov1alpha1.WorkflowTemplate{
					ObjectMeta: metav1.ObjectMeta{Name: workflow.Spec.Template, Namespace: workflow.Namespace},
					Spec: terrakojoiov1alpha1.WorkflowTemplateSpec{
						DisplayName: "Test",
						Match:       terrakojoiov1alpha1.WorkflowMatch{Paths: []string{"**/*"}},
						Job:         newTemplateJobSpec("step", "busybox", []string{"echo"}),
					},
				}
				client := fake.NewClientBuilder().
					WithScheme(testScheme).
					WithStatusSubresource(&terrakojoiov1alpha1.Workflow{}).
					WithObjects(workflow, branch, template).
					Build()
				checkRunID := int64(1)
				ghManager := &fakeGitHubClientManager{
					GetClientForBranchFunc: func(ctx context.Context, branch *terrakojoiov1alpha1.Branch) (gh.ClientInterface, error) {
						return &fakeGitHubClient{CreateCheckRunFunc: func(owner, repo, sha, name string) (*ghapi.CheckRun, error) {
							return &ghapi.CheckRun{ID: &checkRunID}, nil
						}}, nil
					},
				}
				reconciler := &WorkflowReconciler{Client: client, Scheme: testScheme, GitHubClientManager: ghManager}
				return setupResult{reconciler: reconciler, request: makeRequest(workflow)}
			}, "no kind is registered"),
			Entry("fails when creating job errors", func() setupResult {
				testScheme := newWorkflowTestScheme()
				workflow := &terrakojoiov1alpha1.Workflow{
					ObjectMeta: metav1.ObjectMeta{
						Name:       "workflow-create-job-error",
						Namespace:  "default",
						Finalizers: []string{workflowFinalizer},
					},
					Spec: terrakojoiov1alpha1.WorkflowSpec{
						Owner:      "owner",
						Repository: "repo",
						Branch:     "branch-ref",
						SHA:        "0123456789abcdef0123456789abcdef01234567",
						Template:   "template",
						Path:       "path",
					},
				}
				branch := &terrakojoiov1alpha1.Branch{
					ObjectMeta: metav1.ObjectMeta{Name: "branch-ref", Namespace: workflow.Namespace},
					Spec: terrakojoiov1alpha1.BranchSpec{
						Owner:      workflow.Spec.Owner,
						Repository: workflow.Spec.Repository,
						Name:       "feature",
						SHA:        workflow.Spec.SHA,
					},
				}
				template := &terrakojoiov1alpha1.WorkflowTemplate{
					ObjectMeta: metav1.ObjectMeta{Name: workflow.Spec.Template, Namespace: workflow.Namespace},
					Spec: terrakojoiov1alpha1.WorkflowTemplateSpec{
						DisplayName: "Test",
						Match:       terrakojoiov1alpha1.WorkflowMatch{Paths: []string{"**/*"}},
						Job:         newTemplateJobSpec("step", "busybox", []string{"echo"}),
					},
				}
				baseClient := newWorkflowFakeClient(testScheme, workflow, branch, template)
				checkRunID := int64(1)
				ghManager := &fakeGitHubClientManager{
					GetClientForBranchFunc: func(ctx context.Context, branch *terrakojoiov1alpha1.Branch) (gh.ClientInterface, error) {
						return &fakeGitHubClient{CreateCheckRunFunc: func(owner, repo, sha, name string) (*ghapi.CheckRun, error) {
							return &ghapi.CheckRun{ID: &checkRunID}, nil
						}}, nil
					},
				}
				reconciler := &WorkflowReconciler{Client: &jobCreateErrorClient{Client: baseClient, err: fmt.Errorf("create job error")}, Scheme: testScheme, GitHubClientManager: ghManager}
				return setupResult{reconciler: reconciler, request: makeRequest(workflow)}
			}, "create job error"),
			Entry("fails when UpdateCheckRun errors after job creation", func() setupResult {
				testScheme := newWorkflowTestScheme()
				workflow := &terrakojoiov1alpha1.Workflow{
					ObjectMeta: metav1.ObjectMeta{
						Name:       "workflow-update-checkrun-after-job",
						Namespace:  "default",
						Finalizers: []string{workflowFinalizer},
					},
					Spec: terrakojoiov1alpha1.WorkflowSpec{
						Owner:      "owner",
						Repository: "repo",
						Branch:     "branch-ref",
						SHA:        "0123456789abcdef0123456789abcdef01234567",
						Template:   "template",
						Path:       "path",
					},
				}
				branch := &terrakojoiov1alpha1.Branch{
					ObjectMeta: metav1.ObjectMeta{Name: "branch-ref", Namespace: workflow.Namespace},
					Spec: terrakojoiov1alpha1.BranchSpec{
						Owner:      workflow.Spec.Owner,
						Repository: workflow.Spec.Repository,
						Name:       "feature",
						SHA:        workflow.Spec.SHA,
					},
				}
				template := &terrakojoiov1alpha1.WorkflowTemplate{
					ObjectMeta: metav1.ObjectMeta{Name: workflow.Spec.Template, Namespace: workflow.Namespace},
					Spec: terrakojoiov1alpha1.WorkflowTemplateSpec{
						DisplayName: "Test",
						Match:       terrakojoiov1alpha1.WorkflowMatch{Paths: []string{"**/*"}},
						Job:         newTemplateJobSpec("step", "busybox", []string{"echo"}),
					},
				}
				client := newWorkflowFakeClient(testScheme, workflow, branch, template)
				checkRunID := int64(1)
				ghManager := &fakeGitHubClientManager{
					GetClientForBranchFunc: func(ctx context.Context, branch *terrakojoiov1alpha1.Branch) (gh.ClientInterface, error) {
						return &fakeGitHubClient{
							CreateCheckRunFunc: func(owner, repo, sha, name string) (*ghapi.CheckRun, error) {
								return &ghapi.CheckRun{ID: &checkRunID}, nil
							},
							UpdateCheckRunFunc: func(owner, repo string, checkRunID int64, name, status, conclusion string) error {
								return fmt.Errorf("update checkrun error")
							},
						}, nil
					},
				}
				reconciler := &WorkflowReconciler{Client: client, Scheme: testScheme, GitHubClientManager: ghManager}
				return setupResult{reconciler: reconciler, request: makeRequest(workflow)}
			}, "update checkrun error"),
		)
	})

	When("testing helper methods", func() {
		DescribeTable("normalizeContainerName", func(input, expected string) {
			reconciler := &WorkflowReconciler{}
			Expect(reconciler.normalizeContainerName(input)).To(Equal(expected))
		},
			Entry("lowercases and replaces spaces", "Plan Step", "plan-step"),
			Entry("trims to step when empty", "---", "step"),
			Entry("keeps alphanumerics", "step1", "step1"),
			Entry("keeps 63 characters", strings.Repeat("a", 63), strings.Repeat("a", 63)),
			Entry("truncates over 63 characters", strings.Repeat("a", 70), strings.Repeat("a", 63)),
			Entry("trims trailing hyphen after truncation", strings.Repeat("a", 62)+"-b", strings.Repeat("a", 62)),
		)

		It("createJobFromTemplate applies defaults and normalizes container names", func() {
			reconciler := &WorkflowReconciler{}
			template := &terrakojoiov1alpha1.WorkflowTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "template",
					Namespace: "template-ns",
				},
				Spec: terrakojoiov1alpha1.WorkflowTemplateSpec{
					DisplayName: "test",
					Match:       terrakojoiov1alpha1.WorkflowMatch{Paths: []string{"**/*"}},
					Job:         newTemplateJobSpec("Plan Step", "busybox", []string{"echo", "hello"}),
				},
			}

			job := reconciler.createJobFromTemplate("workflow-name", "workflow-ns", template)
			Expect(job.Name).To(Equal("workflow-name"))
			Expect(job.Namespace).To(Equal("workflow-ns"))

			Expect(job.Spec.BackoffLimit).NotTo(BeNil())
			Expect(*job.Spec.BackoffLimit).To(Equal(int32(0)))
			Expect(job.Spec.Template.Spec.RestartPolicy).To(Equal(corev1.RestartPolicyNever))

			Expect(job.Spec.Template.Spec.SecurityContext).NotTo(BeNil())
			Expect(job.Spec.Template.Spec.SecurityContext.RunAsNonRoot).NotTo(BeNil())
			Expect(*job.Spec.Template.Spec.SecurityContext.RunAsNonRoot).To(BeTrue())
			Expect(job.Spec.Template.Spec.SecurityContext.RunAsUser).NotTo(BeNil())
			Expect(*job.Spec.Template.Spec.SecurityContext.RunAsUser).To(Equal(int64(1000)))
			Expect(job.Spec.Template.Spec.SecurityContext.SeccompProfile).NotTo(BeNil())
			Expect(job.Spec.Template.Spec.SecurityContext.SeccompProfile.Type).To(Equal(corev1.SeccompProfileTypeRuntimeDefault))

			Expect(job.Spec.Template.Spec.Containers).To(HaveLen(1))
			Expect(job.Spec.Template.Spec.Containers[0].Name).To(Equal("plan-step"))
			Expect(job.Spec.Template.Spec.Containers[0].SecurityContext).NotTo(BeNil())
			Expect(job.Spec.Template.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation).NotTo(BeNil())
			Expect(*job.Spec.Template.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation).To(BeFalse())
			Expect(job.Spec.Template.Spec.Containers[0].SecurityContext.Capabilities).NotTo(BeNil())
			Expect(job.Spec.Template.Spec.Containers[0].SecurityContext.Capabilities.Drop).To(Equal([]corev1.Capability{"ALL"}))
		})

		It("createJobFromTemplate preserves explicit security settings", func() {
			reconciler := &WorkflowReconciler{}

			backoff := int32(3)
			runAsNonRoot := false
			runAsUser := int64(2000)
			allowPrivilegeEscalation := true
			template := &terrakojoiov1alpha1.WorkflowTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "template",
					Namespace: "template-ns",
				},
				Spec: terrakojoiov1alpha1.WorkflowTemplateSpec{
					DisplayName: "test",
					Match:       terrakojoiov1alpha1.WorkflowMatch{Paths: []string{"**/*"}},
					Job: batchv1.JobSpec{
						BackoffLimit: &backoff,
						Template: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								RestartPolicy: corev1.RestartPolicyOnFailure,
								SecurityContext: &corev1.PodSecurityContext{
									RunAsNonRoot: &runAsNonRoot,
									RunAsUser:    &runAsUser,
									SeccompProfile: &corev1.SeccompProfile{
										Type: corev1.SeccompProfileTypeUnconfined,
									},
								},
								Containers: []corev1.Container{
									{
										Name:  "custom",
										Image: "busybox",
										SecurityContext: &corev1.SecurityContext{
											AllowPrivilegeEscalation: &allowPrivilegeEscalation,
											Capabilities: &corev1.Capabilities{
												Drop: []corev1.Capability{"NET_RAW"},
											},
										},
									},
								},
							},
						},
					},
				},
			}

			job := reconciler.createJobFromTemplate("workflow-name", "workflow-ns", template)
			Expect(job.Spec.BackoffLimit).NotTo(BeNil())
			Expect(*job.Spec.BackoffLimit).To(Equal(int32(3)))
			Expect(job.Spec.Template.Spec.RestartPolicy).To(Equal(corev1.RestartPolicyOnFailure))
			Expect(job.Spec.Template.Spec.SecurityContext).NotTo(BeNil())
			Expect(job.Spec.Template.Spec.SecurityContext.RunAsNonRoot).NotTo(BeNil())
			Expect(*job.Spec.Template.Spec.SecurityContext.RunAsNonRoot).To(BeFalse())
			Expect(job.Spec.Template.Spec.SecurityContext.RunAsUser).NotTo(BeNil())
			Expect(*job.Spec.Template.Spec.SecurityContext.RunAsUser).To(Equal(int64(2000)))
			Expect(job.Spec.Template.Spec.SecurityContext.SeccompProfile).NotTo(BeNil())
			Expect(job.Spec.Template.Spec.SecurityContext.SeccompProfile.Type).To(Equal(corev1.SeccompProfileTypeUnconfined))
			Expect(job.Spec.Template.Spec.Containers).To(HaveLen(1))
			Expect(job.Spec.Template.Spec.Containers[0].Name).To(Equal("custom"))
			Expect(job.Spec.Template.Spec.Containers[0].SecurityContext).NotTo(BeNil())
			Expect(job.Spec.Template.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation).NotTo(BeNil())
			Expect(*job.Spec.Template.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation).To(BeTrue())
			Expect(job.Spec.Template.Spec.Containers[0].SecurityContext.Capabilities).NotTo(BeNil())
			Expect(job.Spec.Template.Spec.Containers[0].SecurityContext.Capabilities.Drop).To(Equal([]corev1.Capability{"NET_RAW"}))
		})

		DescribeTable("determineWorkflowPhase", func(jobStatus batchv1.JobStatus, expected WorkflowPhase) {
			reconciler := &WorkflowReconciler{}
			workflow := &terrakojoiov1alpha1.Workflow{}
			job := &batchv1.Job{Status: jobStatus}
			phase, _ := reconciler.determineWorkflowPhase(workflow, job)
			Expect(phase).To(Equal(expected))
		},
			Entry("succeeded", batchv1.JobStatus{Succeeded: 1}, WorkflowPhaseSucceeded),
			Entry("failed", batchv1.JobStatus{Failed: 1}, WorkflowPhaseFailed),
			Entry("running", batchv1.JobStatus{Active: 1}, WorkflowPhaseRunning),
			Entry("pending", batchv1.JobStatus{}, WorkflowPhasePending),
		)

		DescribeTable("checkRunStatus", func(phase WorkflowPhase, status, conclusion string) {
			reconciler := &WorkflowReconciler{}
			gotStatus, gotConclusion := reconciler.checkRunStatus(phase)
			Expect(gotStatus).To(Equal(status))
			Expect(gotConclusion).To(Equal(conclusion))
		},
			Entry("pending", WorkflowPhasePending, "queued", ""),
			Entry("running", WorkflowPhaseRunning, "in_progress", ""),
			Entry("succeeded", WorkflowPhaseSucceeded, "completed", "success"),
			Entry("failed", WorkflowPhaseFailed, "completed", "failure"),
			Entry("cancelled", WorkflowPhaseCancelled, "completed", "cancelled"),
			Entry("unknown", WorkflowPhase("unknown"), "queued", ""),
		)

		It("setCondition updates existing condition", func() {
			reconciler := &WorkflowReconciler{}
			workflow := &terrakojoiov1alpha1.Workflow{
				Status: terrakojoiov1alpha1.WorkflowStatus{
					Conditions: []metav1.Condition{{Type: "JobStatus", Status: metav1.ConditionFalse}},
				},
			}
			reconciler.setCondition(workflow, "JobStatus", metav1.ConditionTrue, "Running", "job running")
			Expect(workflow.Status.Conditions).To(HaveLen(1))
			Expect(workflow.Status.Conditions[0].Status).To(Equal(metav1.ConditionTrue))
		})

		It("setCondition adds new condition", func() {
			reconciler := &WorkflowReconciler{}
			workflow := &terrakojoiov1alpha1.Workflow{}
			reconciler.setCondition(workflow, "JobStatus", metav1.ConditionTrue, "Running", "job running")
			Expect(workflow.Status.Conditions).To(HaveLen(1))
			Expect(workflow.Status.Conditions[0].Type).To(Equal("JobStatus"))
		})

		It("updateWorkflowStatusWithRetry returns error when workflow get fails", func() {
			testScheme := newWorkflowTestScheme()
			workflow := &terrakojoiov1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: "workflow-get-error", Namespace: "default"},
			}
			baseClient := newWorkflowFakeClient(testScheme, workflow)
			reconciler := &WorkflowReconciler{Client: &workflowGetErrorClient{Client: baseClient, err: fmt.Errorf("get error")}, Scheme: testScheme}
			err := reconciler.updateWorkflowStatusWithRetry(context.Background(), workflow, func(latest *terrakojoiov1alpha1.Workflow) {
				latest.Status.Phase = string(WorkflowPhaseRunning)
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("get error"))
		})

		It("updateWorkflowStatusWithRetry returns error when status update fails", func() {
			testScheme := newWorkflowTestScheme()
			workflow := &terrakojoiov1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: "workflow-status-error", Namespace: "default"},
			}
			baseClient := newWorkflowFakeClient(testScheme, workflow)
			reconciler := &WorkflowReconciler{Client: &statusErrorClient{Client: baseClient, err: fmt.Errorf("status update failed")}, Scheme: testScheme}
			err := reconciler.updateWorkflowStatusWithRetry(context.Background(), workflow, func(latest *terrakojoiov1alpha1.Workflow) {
				latest.Status.Phase = string(WorkflowPhaseRunning)
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("status update failed"))
		})

		It("handleWorkflowDeletion returns nil when CheckRunID is zero", func() {
			testScheme := newWorkflowTestScheme()
			reconciler := &WorkflowReconciler{Client: newWorkflowFakeClient(testScheme), Scheme: testScheme}
			workflow := &terrakojoiov1alpha1.Workflow{}
			err := reconciler.handleWorkflowDeletion(context.Background(), &fakeGitHubClient{}, workflow, "job")
			Expect(err).NotTo(HaveOccurred())
		})
	})

	When("handling workflow deletion", func() {
		It("returns nil without updating checkrun when job is completed", func() {
			ctx := context.Background()
			testScheme := newWorkflowTestScheme()

			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "workflow-complete", Namespace: "default"},
				Status:     batchv1.JobStatus{Succeeded: 1},
			}
			reconciler := &WorkflowReconciler{Client: newWorkflowFakeClient(testScheme, job), Scheme: testScheme}

			workflow := &terrakojoiov1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: "workflow-complete", Namespace: "default"},
				Spec: terrakojoiov1alpha1.WorkflowSpec{
					Owner:      "owner",
					Repository: "repo",
				},
				Status: terrakojoiov1alpha1.WorkflowStatus{
					CheckRunID:   11,
					CheckRunName: "check",
				},
			}

			var updateCalled atomic.Bool
			ghClient := &fakeGitHubClient{
				UpdateCheckRunFunc: func(owner, repo string, checkRunID int64, name, status, conclusion string) error {
					updateCalled.Store(true)
					return nil
				},
			}

			err := reconciler.handleWorkflowDeletion(ctx, ghClient, workflow, job.Name)
			Expect(err).NotTo(HaveOccurred())
			Expect(updateCalled.Load()).To(BeFalse())
		})

		It("returns nil without updating checkrun when job is not found", func() {
			ctx := context.Background()
			testScheme := newWorkflowTestScheme()
			reconciler := &WorkflowReconciler{Client: newWorkflowFakeClient(testScheme), Scheme: testScheme}

			workflow := &terrakojoiov1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: "workflow-missing-job", Namespace: "default"},
				Spec: terrakojoiov1alpha1.WorkflowSpec{
					Owner:      "owner",
					Repository: "repo",
				},
				Status: terrakojoiov1alpha1.WorkflowStatus{
					CheckRunID:   12,
					CheckRunName: "check",
				},
			}

			var updateCalled atomic.Bool
			ghClient := &fakeGitHubClient{
				UpdateCheckRunFunc: func(owner, repo string, checkRunID int64, name, status, conclusion string) error {
					updateCalled.Store(true)
					return nil
				},
			}

			err := reconciler.handleWorkflowDeletion(ctx, ghClient, workflow, "missing-job")
			Expect(err).NotTo(HaveOccurred())
			Expect(updateCalled.Load()).To(BeFalse())
		})

		It("returns an error when job lookup fails", func() {
			ctx := context.Background()
			testScheme := newWorkflowTestScheme()
			baseClient := newWorkflowFakeClient(testScheme)
			reconciler := &WorkflowReconciler{
				Client: &jobGetErrorClient{Client: baseClient, err: fmt.Errorf("job get error")},
				Scheme: testScheme,
			}

			workflow := &terrakojoiov1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: "workflow-job-error", Namespace: "default"},
				Spec: terrakojoiov1alpha1.WorkflowSpec{
					Owner:      "owner",
					Repository: "repo",
				},
				Status: terrakojoiov1alpha1.WorkflowStatus{
					CheckRunID:   13,
					CheckRunName: "check",
				},
			}

			var updateCalled atomic.Bool
			ghClient := &fakeGitHubClient{
				UpdateCheckRunFunc: func(owner, repo string, checkRunID int64, name, status, conclusion string) error {
					updateCalled.Store(true)
					return nil
				},
			}

			err := reconciler.handleWorkflowDeletion(ctx, ghClient, workflow, "job-error")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("job get error"))
			Expect(updateCalled.Load()).To(BeFalse())
		})
	})

	When("setting up manager", func() {
		It("registers the controller with the manager", func() {
			skipNameValidation := true
			mgr, err := ctrl.NewManager(cfg, ctrl.Options{
				Scheme: scheme.Scheme,
				Metrics: server.Options{
					BindAddress: "0",
				},
				HealthProbeBindAddress: "0",
				Controller:             config.Controller{SkipNameValidation: &skipNameValidation},
			})
			Expect(err).NotTo(HaveOccurred())

			reconciler := &WorkflowReconciler{
				Client:              mgr.GetClient(),
				Scheme:              mgr.GetScheme(),
				GitHubClientManager: &fakeGitHubClientManager{},
			}
			Expect(reconciler.SetupWithManager(mgr)).To(Succeed())
		})
	})
})
