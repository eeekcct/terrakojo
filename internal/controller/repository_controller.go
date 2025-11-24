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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	terrakojoiov1alpha1 "github.com/eeekcct/terrakojo/api/v1alpha1"
	"github.com/eeekcct/terrakojo/internal/kubernetes"
)

// RepositoryReconciler reconciles a Repository object
type RepositoryReconciler struct {
	client.Client
	Scheme              *runtime.Scheme
	GitHubClientManager kubernetes.GitHubClientManagerInterface
}

// +kubebuilder:rbac:groups=terrakojo.io,resources=repositories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=terrakojo.io,resources=repositories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=terrakojo.io,resources=repositories/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Repository object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.1/pkg/reconcile
func (r *RepositoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Get the Repository resource
	var repo terrakojoiov1alpha1.Repository
	if err := r.Get(ctx, req.NamespacedName, &repo); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Ensure required labels are present
	if err := r.ensureLabels(ctx, &repo); err != nil {
		log.Error(err, "Failed to ensure labels")
		return ctrl.Result{}, err
	}

	// Test GitHub authentication if manager is available
	if r.GitHubClientManager == nil {
		log.Error(nil, "GitHubClientManager not initialized")
		return ctrl.Result{}, fmt.Errorf("GitHubClientManager not initialized")
	}

	ghClient, err := r.GitHubClientManager.GetClientForRepository(ctx, &repo)
	if err != nil {
		log.Error(err, "Failed to create GitHub client for repository")
		return ctrl.Result{}, err
	} else {
		log.Info("Successfully created GitHub client for repository",
			"repository", repo.Name,
			"owner", repo.Spec.Owner,
			"secretName", repo.Spec.GitHubSecretRef.Name)
		// Store client for potential later use
		_ = ghClient
	}

	// Get existing Branch resources owned by this Repository
	var branchList terrakojoiov1alpha1.BranchList
	if err := r.List(ctx, &branchList, client.InNamespace(req.Namespace)); err != nil {
		log.Error(err, "Failed to list Branch resources")
		return ctrl.Result{}, err
	}

	// Filter branches that are owned by this repository (using OwnerReference)
	existingBranches := make(map[string]terrakojoiov1alpha1.Branch)
	for _, branch := range branchList.Items {
		// Check if this branch is owned by the current repository
		if r.isOwnedByRepository(&branch, &repo) {
			existingBranches[branch.Spec.Name] = branch
		}
	}

	// Sync BranchRefs with Branch resources
	if err := r.syncBranches(ctx, &repo, existingBranches); err != nil {
		log.Error(err, "Failed to sync branches")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// ensureLabels ensures that the Repository has the required labels for efficient querying
func (r *RepositoryReconciler) ensureLabels(ctx context.Context, repo *terrakojoiov1alpha1.Repository) error {
	log := logf.FromContext(ctx)

	if repo.Labels == nil {
		repo.Labels = make(map[string]string)
	}

	needsUpdate := false

	// Ensure owner label
	ownerLabel := "terrakojo.io/owner"
	if repo.Labels[ownerLabel] != repo.Spec.Owner {
		repo.Labels[ownerLabel] = repo.Spec.Owner
		needsUpdate = true
	}

	// Ensure repository name label
	repoNameLabel := "terrakojo.io/repo-name"
	if repo.Labels[repoNameLabel] != repo.Spec.Name {
		repo.Labels[repoNameLabel] = repo.Spec.Name
		needsUpdate = true
	}

	// Ensure managed-by label
	managedByLabel := "app.kubernetes.io/managed-by"
	managedByValue := "terrakojo-controller"
	if repo.Labels[managedByLabel] != managedByValue {
		repo.Labels[managedByLabel] = managedByValue
		needsUpdate = true
	}

	if needsUpdate {
		log.Info("Updating Repository labels", "name", repo.Name, "namespace", repo.Namespace)
		if err := r.Update(ctx, repo); err != nil {
			return fmt.Errorf("failed to update Repository labels: %w", err)
		}
	}

	return nil
}

// syncBranches synchronizes BranchRefs in Repository status with actual Branch resources
func (r *RepositoryReconciler) syncBranches(ctx context.Context, repo *terrakojoiov1alpha1.Repository, existingBranches map[string]terrakojoiov1alpha1.Branch) error {
	log := logf.FromContext(ctx)

	// 1. Create Branch resources for BranchRefs that don't exist
	branchRefsMap := make(map[string]bool)
	for _, branch := range repo.Status.BranchList {
		branchRefsMap[branch.Ref] = true
		if _, exists := existingBranches[branch.Ref]; !exists {
			if err := r.createBranchResource(ctx, repo, branch); err != nil {
				log.Error(err, "Failed to create Branch resource", "branch", branch.Ref)
				return err
			}
			log.Info("Created Branch resource", "branch", branch.Ref, "namespace", repo.Namespace)
		}
	}

	// 2. Delete Branch resources that are not in BranchRefs
	for branchName, branch := range existingBranches {
		if !branchRefsMap[branchName] {
			if err := r.Delete(ctx, &branch); err != nil {
				log.Error(err, "Failed to delete Branch resource", "branch", branchName)
				return err
			}
			log.Info("Deleted Branch resource", "branch", branchName, "namespace", repo.Namespace)
		}
	}

	return nil
}

// createBranchResource creates a new Branch resource
func (r *RepositoryReconciler) createBranchResource(ctx context.Context, repo *terrakojoiov1alpha1.Repository, branch terrakojoiov1alpha1.BranchInfo) error {
	newBranch := &terrakojoiov1alpha1.Branch{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", repo.Spec.Name, branch.Ref),
			Namespace: repo.Namespace,
		},
		Spec: terrakojoiov1alpha1.BranchSpec{
			Owner:      repo.Spec.Owner,
			Repository: repo.Spec.Name,
			Name:       branch.Ref,
			PRNumber:   branch.PRNumber,
			SHA:        branch.SHA,
		},
	}

	// Set Repository as the owner of the Branch
	if err := controllerutil.SetControllerReference(repo, newBranch, r.Scheme); err != nil {
		return fmt.Errorf("failed to set controller reference: %w", err)
	}

	return r.Create(ctx, newBranch)
}

// isOwnedByRepository checks if a Branch is owned by the given Repository
func (r *RepositoryReconciler) isOwnedByRepository(branch *terrakojoiov1alpha1.Branch, repo *terrakojoiov1alpha1.Repository) bool {
	for _, ownerRef := range branch.GetOwnerReferences() {
		if ownerRef.UID == repo.UID && ownerRef.Kind == "Repository" {
			return true
		}
	}
	return false
}

// SetupWithManager sets up the controller with the Manager.
func (r *RepositoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&terrakojoiov1alpha1.Repository{}).
		Owns(&terrakojoiov1alpha1.Branch{}).
		Named("repository").
		Complete(r)
}
