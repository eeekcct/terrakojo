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

var _ = Describe("Repository Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default", // TODO(user):Modify as needed
		}
		repository := &terrakojoiov1alpha1.Repository{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind Repository")
			err := k8sClient.Get(ctx, typeNamespacedName, repository)
			if err != nil && errors.IsNotFound(err) {
				resource := &terrakojoiov1alpha1.Repository{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
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
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
			Expect(k8sClient.Get(ctx, typeNamespacedName, repository)).To(Succeed())
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &terrakojoiov1alpha1.Repository{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if errors.IsNotFound(err) {
				return
			}
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance Repository")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should create the resource with required spec fields", func() {
			By("Fetching the created resource")
			fetched := &terrakojoiov1alpha1.Repository{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, fetched)).To(Succeed())
			Expect(fetched.Spec.Owner).To(Equal("test-owner"))
			Expect(fetched.Spec.Name).To(Equal("test-repo"))
			Expect(fetched.Spec.Type).To(Equal("github"))
			Expect(fetched.Spec.DefaultBranch).To(Equal("main"))
			Expect(fetched.Spec.GitHubSecretRef.Name).To(Equal("github-secret"))
		})
	})
})
