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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	terrakojoiov1alpha1 "github.com/eeekcct/terrakojo/api/v1alpha1"
)

var _ = Describe("Branch Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default", // TODO(user):Modify as needed
		}
		branch := &terrakojoiov1alpha1.Branch{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind Branch")
			err := k8sClient.Get(ctx, typeNamespacedName, branch)
			if err != nil && errors.IsNotFound(err) {
				resource := &terrakojoiov1alpha1.Branch{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: terrakojoiov1alpha1.BranchSpec{
						Owner:      "test-owner",
						Repository: "test-repo",
						Name:       "main",
						SHA:        "0123456789abcdef0123456789abcdef01234567",
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
			Expect(k8sClient.Get(ctx, typeNamespacedName, branch)).To(Succeed())
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &terrakojoiov1alpha1.Branch{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if errors.IsNotFound(err) {
				return
			}
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance Branch")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should create the resource with required spec fields", func() {
			By("Fetching the created resource")
			fetched := &terrakojoiov1alpha1.Branch{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, fetched)).To(Succeed())
			Expect(fetched.Spec.Owner).To(Equal("test-owner"))
			Expect(fetched.Spec.Repository).To(Equal("test-repo"))
			Expect(fetched.Spec.Name).To(Equal("main"))
		})
	})
})
