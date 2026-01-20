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
	"time"

	terrakojoiov1alpha1 "github.com/eeekcct/terrakojo/api/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

var _ = Describe("Branch Controller", func() {
	var (
		mgr       ctrl.Manager
		mgrCtx    context.Context
		mgrCancel context.CancelFunc
		mgrErrCh  chan error
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

		reconciler := &BranchReconciler{
			Client:              mgr.GetClient(),
			Scheme:              mgr.GetScheme(),
			GitHubClientManager: &fakeGitHubClientManager{},
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

		It("should create a Branch resource for each branch in the repository", func() {
			ctx := context.Background()
			branch := &terrakojoiov1alpha1.Branch{
				ObjectMeta: ctrl.ObjectMeta{
					Name:      "example-branch",
					Namespace: namespace,
				},
				Spec: terrakojoiov1alpha1.BranchSpec{
					Repository: "example-repo",
					Owner:      "example-owner",
					Name:       "example-branch",
					SHA:        "0123456789abcdef0123456789abcdef01234567",
				},
			}
			Expect(k8sClient.Create(ctx, branch)).To(Succeed())

			Eventually(func(g Gomega) {
				key := types.NamespacedName{
					Name:      branch.Name,
					Namespace: namespace,
				}
				fetched := &terrakojoiov1alpha1.Branch{}
				g.Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
				g.Expect(fetched.Finalizers).To(ContainElement(branchFinalizer))
			}).WithTimeout(5 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
		})
	})
})
