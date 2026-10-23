package controller

import (
	"context"
	"strings"

	terrakojoiov1alpha1 "github.com/eeekcct/terrakojo/api/v1alpha1"
	"github.com/eeekcct/terrakojo/internal/repository/sync"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("Fork branch identity persistence", func() {
	It("keeps local and fork objects separate through update and closure", func() {
		ctx := context.Background()
		repo := newTestRepository("fork-identity-envtest", "")
		Expect(k8sClient.Create(ctx, repo)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, repo)).To(Succeed()) })
		reconciler := &RepositoryReconciler{Client: k8sClient, Scheme: scheme.Scheme}
		snapshot := &sync.BranchHeads{ExternalPRs: map[int]string{10: "feature", 11: "feature"}}
		for _, number := range []int{0, 10, 11} {
			snapshot.Heads = append(snapshot.Heads, sync.BranchHead{BranchInfo: terrakojoiov1alpha1.BranchInfo{Ref: "feature", SHA: forkTestSHA, PRNumber: number}, ExternalPR: number != 0})
		}
		list := func() map[int]terrakojoiov1alpha1.Branch {
			var branches terrakojoiov1alpha1.BranchList
			Expect(k8sClient.List(ctx, &branches, client.InNamespace(repo.Namespace), client.MatchingLabels{"terrakojo.io/repo-uid": string(repo.UID)})).To(Succeed())
			result := map[int]terrakojoiov1alpha1.Branch{}
			for _, branch := range branches.Items {
				result[branch.Spec.PRNumber] = branch
			}
			Expect(result).To(HaveLen(len(branches.Items)))
			return result
		}
		reconcile := func() {
			existing := map[string][]terrakojoiov1alpha1.Branch{}
			for _, branch := range list() {
				existing[branchIdentity(branch)] = append(existing[branchIdentity(branch)], branch)
			}
			Expect(reconciler.syncBranchHeads(ctx, repo, existing, snapshot)).To(Succeed())
		}
		reconcile()
		before := list()
		Expect(before).To(HaveLen(3))
		Expect(before[10].UID).NotTo(Equal(before[11].UID))
		Expect(before[0].UID).NotTo(Equal(before[10].UID))
		for _, branch := range before {
			Expect(branch.Spec.Name).To(Equal("feature"))
			Expect(branch.Spec.Owner).To(Equal(repo.Spec.Owner))
			Expect(metav1.GetControllerOf(&branch).UID).To(Equal(repo.UID))
		}
		snapshot.Heads[1].SHA = strings.Repeat("b", 40)
		reconcile()
		after := list()
		Expect(after[0].UID).To(Equal(before[0].UID))
		Expect(after[11].UID).To(Equal(before[11].UID))
		Expect(after[10].UID).NotTo(Equal(before[10].UID))
		snapshot.Heads = append(snapshot.Heads[:1], snapshot.Heads[2:]...)
		delete(snapshot.ExternalPRs, 10)
		reconcile()
		after = list()
		Expect(after).To(HaveLen(2))
		Expect(after[0].UID).To(Equal(before[0].UID))
		Expect(after[11].UID).To(Equal(before[11].UID))
		for _, branch := range after {
			Expect(k8sClient.Delete(ctx, &branch)).To(Succeed())
		}
	})
})
