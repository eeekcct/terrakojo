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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	terrakojoiov1alpha1 "github.com/eeekcct/terrakojo/api/v1alpha1"
	gh "github.com/eeekcct/terrakojo/internal/github"
	"github.com/eeekcct/terrakojo/internal/kubernetes"
	"github.com/eeekcct/terrakojo/internal/repository/sync"
)

// RepositoryReconciler reconciles a Repository object
type RepositoryReconciler struct {
	client.Client
	Scheme              *runtime.Scheme
	GitHubClientManager kubernetes.GitHubClientManagerInterface
}

const repositoryFinalizer = "terrakojo.io/cleanup-branches"

// repositorySyncInterval is the interval at which the controller polls GitHub for changes.
// Webhooks should be the primary trigger for syncs, so this is kept long to minimize
// API rate limit usage. The annotation-based sync request from webhooks provides
// immediate sync when changes occur, but if webhooks are missed or fail, there can be
// a delay of up to this interval before a repository is synced again.
const repositorySyncInterval = 30 * time.Minute

// +kubebuilder:rbac:groups=terrakojo.io,resources=repositories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=terrakojo.io,resources=repositories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=terrakojo.io,resources=repositories/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch

// +kubebuilder:rbac:groups=terrakojo.io,resources=branches,verbs=get;list;watch;create;update;patch;delete

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

	// Handle deletion and cleanup owned Branches first
	if !repo.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&repo, repositoryFinalizer) {
			if res, err := r.handleRepositoryDeletion(ctx, &repo); err != nil {
				log.Error(err, "Failed to cleanup branches before deleting repository")
				return ctrl.Result{}, err
			} else if res.RequeueAfter > 0 {
				return res, nil
			}
			controllerutil.RemoveFinalizer(&repo, repositoryFinalizer)
			if err := r.Update(ctx, &repo); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Ensure finalizer to guarantee branch cleanup
	if !controllerutil.ContainsFinalizer(&repo, repositoryFinalizer) {
		controllerutil.AddFinalizer(&repo, repositoryFinalizer)
		if err := r.Update(ctx, &repo); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Ensure required labels are present
	labelsUpdated, err := r.ensureLabels(ctx, &repo)
	if err != nil {
		log.Error(err, "Failed to ensure labels")
		return ctrl.Result{}, err
	}

	// If labels were updated, end this reconcile and let the next one handle the business logic
	if labelsUpdated {
		return ctrl.Result{}, nil
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
			"owner", repo.Spec.Owner,
			"secretName", repo.Spec.GitHubSecretRef.Name)
	}

	var branchList terrakojoiov1alpha1.BranchList
	if err := r.List(
		ctx,
		&branchList,
		client.InNamespace(req.Namespace),
		client.MatchingFields{"metadata.ownerReferences.uid": string(repo.UID)},
	); err != nil {
		log.Error(err, "Failed to list Branch resources")
		return ctrl.Result{}, err
	}

	// Build a map for quick lookup
	existingBranches := make(map[string][]terrakojoiov1alpha1.Branch)
	for _, branch := range branchList.Items {
		existingBranches[branch.Spec.Name] = append(existingBranches[branch.Spec.Name], branch)
	}

	// Sync branches directly from GitHub data.
	if err := r.syncFromGitHub(ctx, &repo, ghClient, existingBranches); err != nil {
		log.Error(err, "Failed to sync branches from GitHub")
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: repositorySyncInterval}, nil
}

func (r *RepositoryReconciler) syncFromGitHub(ctx context.Context, repo *terrakojoiov1alpha1.Repository, ghClient gh.ClientInterface, existingBranches map[string][]terrakojoiov1alpha1.Branch) error {
	defaultRef := repo.Spec.DefaultBranch
	defaultHeadSHA, err := sync.FetchDefaultBranchHeadSHA(repo, ghClient)
	if err != nil {
		return err
	}

	if defaultHeadSHA != "" {
		commitSHAs, err := sync.CollectDefaultBranchCommits(repo, ghClient, defaultHeadSHA)
		if err != nil {
			return err
		}
		if err := r.ensureDefaultBranchCommits(ctx, repo, commitSHAs, existingBranches[defaultRef]); err != nil {
			return err
		}

		// Update LastDefaultBranchHeadSHA to the last processed commit
		// When >250 commits exist, this will be the 250th commit, not defaultHeadSHA
		// Next reconcile will process remaining commits incrementally
		newLastSHA := defaultHeadSHA
		if len(commitSHAs) > 0 {
			newLastSHA = commitSHAs[len(commitSHAs)-1]
		}
		if repo.Status.LastDefaultBranchHeadSHA != newLastSHA {
			repo.Status.LastDefaultBranchHeadSHA = newLastSHA
			if err := r.Status().Update(ctx, repo); err != nil {
				return err
			}
		}
	}

	branchHeads, err := sync.FetchBranchHeadsFromGitHub(repo, ghClient)
	if err != nil {
		return err
	}
	if err := r.syncBranchHeads(ctx, repo, existingBranches, branchHeads); err != nil {
		return err
	}

	return nil
}

// ensureLabels ensures that the Repository has the required labels for efficient querying
// Returns true if labels were updated (indicating reconcile should end)
func (r *RepositoryReconciler) ensureLabels(ctx context.Context, repo *terrakojoiov1alpha1.Repository) (bool, error) {
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

	// Ensure repository UID label (used for branch selection)
	repoUIDLabel := "terrakojo.io/repo-uid"
	if repo.Labels[repoUIDLabel] != string(repo.UID) {
		repo.Labels[repoUIDLabel] = string(repo.UID)
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
		log.Info("Updating Repository labels")
		if err := r.Update(ctx, repo); err != nil {
			return false, fmt.Errorf("failed to update Repository labels: %w", err)
		}
		return true, nil // Labels were updated, end this reconcile
	}

	return false, nil // No update needed, continue with reconcile
}

func (r *RepositoryReconciler) ensureDefaultBranchCommits(ctx context.Context, repo *terrakojoiov1alpha1.Repository, commitSHAs []string, existing []terrakojoiov1alpha1.Branch) error {
	log := logf.FromContext(ctx)

	if len(commitSHAs) == 0 {
		return nil
	}

	existingBySHA := make(map[string]struct{}, len(existing))
	for _, branch := range existing {
		if branch.Spec.SHA == "" {
			continue
		}
		existingBySHA[branch.Spec.SHA] = struct{}{}
	}

	for _, sha := range commitSHAs {
		if sha == "" {
			continue
		}
		if _, found := existingBySHA[sha]; found {
			continue
		}
		branchInfo := terrakojoiov1alpha1.BranchInfo{
			Ref: repo.Spec.DefaultBranch,
			SHA: sha,
		}
		if err := r.createBranchResource(ctx, repo, branchInfo); err != nil {
			return fmt.Errorf("failed to create branch for default branch commit %s: %w", sha, err)
		}
		log.Info("Created Branch for default branch commit", "ref", repo.Spec.DefaultBranch, "sha", sha)
	}

	return nil
}

func (r *RepositoryReconciler) syncBranchHeads(ctx context.Context, repo *terrakojoiov1alpha1.Repository, branches map[string][]terrakojoiov1alpha1.Branch, desired []terrakojoiov1alpha1.BranchInfo) error {
	log := logf.FromContext(ctx)
	defaultRef := repo.Spec.DefaultBranch

	for _, branchInfo := range desired {
		if branchInfo.Ref == defaultRef {
			continue
		}
		existingForRef := branches[branchInfo.Ref]
		if err := r.ensureBranchResource(ctx, repo, branchInfo, existingForRef); err != nil {
			return err
		}
		delete(branches, branchInfo.Ref)
	}

	// Remove default branch from consideration to avoid deleting commit branches.
	delete(branches, defaultRef)

	for _, staleBranches := range branches {
		for _, branch := range staleBranches {
			if err := r.Delete(ctx, &branch); err != nil && client.IgnoreNotFound(err) != nil {
				return err
			}
			log.Info("Deleted Branch resource no longer desired", "branch", branch.Spec.Name, "sha", branch.Spec.SHA)
		}
	}

	return nil
}

// ensureBranchResource creates or updates a Branch resource as needed
func (r *RepositoryReconciler) ensureBranchResource(ctx context.Context, repo *terrakojoiov1alpha1.Repository, branchInfo terrakojoiov1alpha1.BranchInfo, existing []terrakojoiov1alpha1.Branch) error {
	log := logf.FromContext(ctx)

	// GitHub sync ensures only the latest SHA per ref, so existing should have at most one entry.
	if len(existing) > 0 {
		current := &existing[0]
		if current.Spec.SHA == branchInfo.SHA {
			// Branch already exists with correct SHA, check if metadata needs update
			if r.needsBranchUpdate(current, branchInfo) {
				if err := r.updateBranchResource(ctx, current, branchInfo); err != nil {
					return fmt.Errorf("failed to update branch: %w", err)
				}
				log.Info("Updated Branch resource metadata",
					"branch", branchInfo.Ref,
					"sha", branchInfo.SHA,
					"prNumber", branchInfo.PRNumber)
			}
			return nil
		}
		// SHA changed, delete old Branch (new one will be created below)
		if err := r.Delete(ctx, current); err != nil && client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("failed to delete branch with old SHA %s: %w", current.Name, err)
		}
		log.Info("Deleted Branch resource with old SHA",
			"branch", branchInfo.Ref,
			"oldSHA", current.Spec.SHA,
			"newSHA", branchInfo.SHA)
	}

	// Create new Branch
	if err := r.createBranchResource(ctx, repo, branchInfo); err != nil {
		return fmt.Errorf("failed to create branch: %w", err)
	}
	log.Info("Created Branch resource", "branch", branchInfo.Ref, "sha", branchInfo.SHA)
	return nil
}

// createBranchResource creates a new Branch resource
func (r *RepositoryReconciler) createBranchResource(ctx context.Context, repo *terrakojoiov1alpha1.Repository, branch terrakojoiov1alpha1.BranchInfo) error {
	short := shortSHA(branch.SHA)
	// Hash the ref to avoid collisions when different refs point to the same SHA
	// and to keep names short (branch name will have workflow/job suffixes appended)
	refHash := hashRef(branch.Ref)
	branchName := fmt.Sprintf("%s-%s-%s", repo.Spec.Name, refHash, short)
	newBranch := &terrakojoiov1alpha1.Branch{
		ObjectMeta: metav1.ObjectMeta{
			Name:      branchName,
			Namespace: repo.Namespace,
			Labels: map[string]string{
				"terrakojo.io/repo-uid":  string(repo.UID),
				"terrakojo.io/repo-name": repo.Spec.Name,
			},
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

// needsBranchUpdate checks if a Branch resource needs to be updated
func (r *RepositoryReconciler) needsBranchUpdate(branch *terrakojoiov1alpha1.Branch, branchInfo terrakojoiov1alpha1.BranchInfo) bool {
	return branch.Spec.PRNumber != branchInfo.PRNumber
}

// updateBranchResource updates an existing Branch resource
func (r *RepositoryReconciler) updateBranchResource(ctx context.Context, barnch *terrakojoiov1alpha1.Branch, branchInfo terrakojoiov1alpha1.BranchInfo) error {
	barnch.Spec.PRNumber = branchInfo.PRNumber
	return r.Update(ctx, barnch)
}

// handleRepositoryDeletion removes all Branch resources owned by the Repository to avoid orphaned dependents.
func (r *RepositoryReconciler) handleRepositoryDeletion(ctx context.Context, repo *terrakojoiov1alpha1.Repository) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if err := r.deleteBranchesForRepository(ctx, repo); err != nil {
		return ctrl.Result{}, err
	}

	// Ensure all branches are actually gone before removing the finalizer
	var remaining terrakojoiov1alpha1.BranchList
	if err := r.List(ctx, &remaining,
		client.InNamespace(repo.Namespace),
		client.MatchingFields{"metadata.ownerReferences.uid": string(repo.UID)},
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list branches while waiting for deletion: %w", err)
	}
	if len(remaining.Items) > 0 {
		log.Info("Waiting for branches to be removed before finalizing repository", "remainingBranches", len(remaining.Items))
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	log.Info("Cleaned up branches before repository deletion")
	return ctrl.Result{}, nil
}

// deleteBranchesForRepository lists branches via field index and deletes them individually.
func (r *RepositoryReconciler) deleteBranchesForRepository(ctx context.Context, repo *terrakojoiov1alpha1.Repository) error {
	var branchList terrakojoiov1alpha1.BranchList
	if err := r.List(ctx, &branchList,
		client.InNamespace(repo.Namespace),
		client.MatchingFields{"metadata.ownerReferences.uid": string(repo.UID)},
	); err != nil {
		return fmt.Errorf("failed to list branches for repository: %w", err)
	}

	for _, b := range branchList.Items {
		if delErr := r.Delete(ctx, &b); delErr != nil && client.IgnoreNotFound(delErr) != nil {
			return fmt.Errorf("failed to delete branch %s: %w", b.Name, delErr)
		}
	}

	return nil
}

func shortSHA(sha string) string {
	if len(sha) >= 8 {
		return sha[:8]
	}
	return sha
}

// hashRef creates a short hash of the ref name to ensure uniqueness while keeping names short
func hashRef(ref string) string {
	h := sha256.Sum256([]byte(ref))
	return hex.EncodeToString(h[:])[:8]
}

func indexByOwnerRepositoryUID(obj client.Object) []string {
	branch := obj.(*terrakojoiov1alpha1.Branch)
	for _, ref := range branch.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller && ref.Kind == "Repository" {
			return []string{string(ref.UID)}
		}
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *RepositoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Index Branch resources by their controlling Repository UID for efficient lookups
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&terrakojoiov1alpha1.Branch{},
		"metadata.ownerReferences.uid",
		indexByOwnerRepositoryUID,
	); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&terrakojoiov1alpha1.Repository{}).
		// Owns(&terrakojoiov1alpha1.Branch{}).
		Named("repository").
		Complete(r)
}
