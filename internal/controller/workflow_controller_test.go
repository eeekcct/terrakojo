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
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	terrakojoiov1alpha1 "github.com/eeekcct/terrakojo/api/v1alpha1"
)

var _ = Describe("Workflow Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default", // TODO(user):Modify as needed
		}
		workflow := &terrakojoiov1alpha1.Workflow{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind Workflow")
			err := k8sClient.Get(ctx, typeNamespacedName, workflow)
			if err != nil && errors.IsNotFound(err) {
				resource := &terrakojoiov1alpha1.Workflow{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: terrakojoiov1alpha1.WorkflowSpec{
						Owner:      "test-owner",
						Repository: "test-repo",
						Branch:     "main",
						SHA:        "0123456789abcdef0123456789abcdef01234567",
						Template:   "test-template",
						Path:       "infra/path",
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
			Expect(k8sClient.Get(ctx, typeNamespacedName, workflow)).To(Succeed())
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &terrakojoiov1alpha1.Workflow{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if errors.IsNotFound(err) {
				return
			}
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance Workflow")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should update workflow status", func() {
			By("Updating workflow status")
			controllerReconciler := &WorkflowReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			err := controllerReconciler.updateWorkflowStatus(ctx, workflow, WorkflowPhaseRunning)
			Expect(err).NotTo(HaveOccurred())

			updated := &terrakojoiov1alpha1.Workflow{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(string(WorkflowPhaseRunning)))
			Expect(updated.Status.Conditions).NotTo(BeEmpty())
		})

		It("should retry status updates on conflict", func() {
			By("Updating workflow status with a forced conflict")
			controllerReconciler := &WorkflowReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			var attempts int32
			var conflictInjected int32
			err := controllerReconciler.updateWorkflowStatusWithRetry(ctx, workflow, func(latest *terrakojoiov1alpha1.Workflow) {
				atomic.AddInt32(&attempts, 1)
				if atomic.CompareAndSwapInt32(&conflictInjected, 0, 1) {
					other := latest.DeepCopy()
					other.Status.CheckRunName = "conflict"
					Expect(controllerReconciler.Status().Update(ctx, other)).To(Succeed())
				}
				latest.Status.Phase = string(WorkflowPhaseRunning)
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(attempts).To(BeNumerically(">=", 2))

			updated := &terrakojoiov1alpha1.Workflow{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(string(WorkflowPhaseRunning)))
		})
	})
})
