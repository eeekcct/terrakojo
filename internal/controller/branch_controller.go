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
	"crypto/rand"
	"fmt"
	"path"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"slices"

	terrakojoiov1alpha1 "github.com/eeekcct/terrakojo/api/v1alpha1"
	"github.com/eeekcct/terrakojo/internal/kubernetes"
)

// BranchReconciler reconciles a Branch object
type BranchReconciler struct {
	client.Client
	Scheme              *runtime.Scheme
	GitHubClientManager kubernetes.GitHubClientManagerInterface
}

const branchFinalizer = "terrakojo.io/cleanup-workflows"

// +kubebuilder:rbac:groups=terrakojo.io,resources=branches,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=terrakojo.io,resources=branches/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=terrakojo.io,resources=branches/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=terrakojo.io,resources=repositories,verbs=get;list;watch

// +kubebuilder:rbac:groups=terrakojo.io,resources=workflowtemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups=terrakojo.io,resources=workflows,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Branch object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.1/pkg/reconcile
func (r *BranchReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var branch terrakojoiov1alpha1.Branch
	if err := r.Get(ctx, req.NamespacedName, &branch); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion: ensure workflows are removed before allowing branch GC
	if !branch.ObjectMeta.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&branch, branchFinalizer) {
			if err := r.deleteWorkflowsForBranch(ctx, &branch); err != nil {
				log.Error(err, "Failed to delete workflows while finalizing branch")
				return ctrl.Result{}, err
			}

			// Wait until all workflows owned by this branch are actually gone (finalizers cleared)
			remaining, err := r.listWorkflowsForBranch(ctx, &branch)
			if err != nil {
				log.Error(err, "Failed to list remaining workflows while finalizing branch")
				return ctrl.Result{}, err
			}
			if len(remaining) > 0 {
				log.Info("Waiting for workflows to finish before deleting branch", "remainingWorkflows", len(remaining))
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}

			controllerutil.RemoveFinalizer(&branch, branchFinalizer)
			if err := r.Update(ctx, &branch); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Ensure finalizer so we can clean workflows on branch deletion
	if !controllerutil.ContainsFinalizer(&branch, branchFinalizer) {
		controllerutil.AddFinalizer(&branch, branchFinalizer)
		if err := r.Update(ctx, &branch); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// If all workflows owned by this branch are completed, clean up Repository status and delete the Branch.
	workflows, err := r.listWorkflowsForBranch(ctx, &branch)
	if err != nil {
		log.Error(err, "Failed to list workflows for branch")
		return ctrl.Result{}, err
	}
	if len(workflows) > 0 && allWorkflowsCompleted(workflows) {
		if err := r.cleanupRepositoryStatus(ctx, &branch); err != nil {
			log.Error(err, "Failed to cleanup repository status for completed branch")
			return ctrl.Result{}, err
		}
		if err := r.Delete(ctx, &branch); err != nil && client.IgnoreNotFound(err) != nil {
			log.Error(err, "Failed to delete completed branch")
			return ctrl.Result{}, err
		}
		log.Info("Deleted Branch after all workflows completed",
			"branchName", branch.Spec.Name,
			"sha", branch.Spec.SHA)
		return ctrl.Result{}, nil
	}

	// Deplicate: Check if the SHA has changed since last reconcile
	lastSHA := branch.Annotations["terrakojo.io/last-sha"]
	if lastSHA == branch.Spec.SHA {
		// No changes in SHA, nothing to do
		return ctrl.Result{}, nil
	}

	// If SHA has changed, delete all existing workflows for this branch
	if lastSHA != "" && lastSHA != branch.Spec.SHA {
		if err := r.deleteWorkflowsForBranch(ctx, &branch); err != nil {
			log.Error(err, "Failed to delete existing workflows for branch",
				"oldSHA", lastSHA,
				"newSHA", branch.Spec.SHA)
			return ctrl.Result{}, err
		}
		log.Info("Deleted existing workflows for branch due to SHA change",
			"oldSHA", lastSHA,
			"newSHA", branch.Spec.SHA)
	}

	// Get GitHub client using manager (required)
	if r.GitHubClientManager == nil {
		log.Error(nil, "GitHubClientManager not initialized")
		return ctrl.Result{}, fmt.Errorf("GitHubClientManager not initialized")
	}

	ghClient, err := r.GitHubClientManager.GetClientForBranch(ctx, &branch)
	if err != nil {
		log.Error(err, "Failed to create GitHub client for branch",
			"owner", branch.Spec.Owner,
			"repository", branch.Spec.Repository)
		return ctrl.Result{}, err
	}

	changedFiles, err := ghClient.GetChangedFiles(branch.Spec.Owner, branch.Spec.Repository, branch.Spec.PRNumber)
	if err != nil {
		log.Error(err, "unable to get changed files from GitHub")
		return ctrl.Result{}, err
	}
	if len(changedFiles) == 0 {
		return ctrl.Result{}, nil
	}

	var templates terrakojoiov1alpha1.WorkflowTemplateList
	if err := r.List(ctx, &templates, &client.ListOptions{
		Namespace: branch.Namespace,
	}); err != nil {
		log.Error(err, "unable to list WorkflowTemplates")
		r.setCondition(
			&branch,
			"WorkflowCreateFailed",
			metav1.ConditionFalse,
			"TemplateListFailed",
			"Failed to list WorkflowTemplates",
		)
		if err := r.Status().Update(ctx, &branch); err != nil {
			log.Error(err, "unable to update Branch status after failing to list WorkflowTemplates")
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	groups := matchTemplates(templates, changedFiles)
	if len(groups) == 0 {
		log.Info("No matching WorkflowTemplate found for Branch", "changedFiles", changedFiles)
		return ctrl.Result{}, nil
	}

	workflowNames := []string{}
	for templateName, files := range groups {
		folders := splitFolderLevel(files)
		if len(folders) == 0 {
			continue
		}
		for _, folder := range folders {
			// Generate random workflow name
			randomSuffix, err := generateRandomString(8)
			if err != nil {
				log.Error(err, "Failed to generate random string for workflow name")
				return ctrl.Result{}, err
			}
			wfName := fmt.Sprintf("%s-%s-%s", branch.Name, templateName, randomSuffix)

			if err := r.Get(ctx, client.ObjectKey{Name: wfName, Namespace: branch.Namespace}, &terrakojoiov1alpha1.Workflow{}); err == nil {
				// Workflow already exists, nothing to do
				return ctrl.Result{}, nil
			}

			if err := r.createWorkflowForBranch(ctx, &branch, templateName, wfName, folder); err != nil {
				log.Error(err, "Failed to create Workflow for Branch")
				return ctrl.Result{}, err
			}
			workflowNames = append(workflowNames, wfName)
		}
	}

	log.Info("Created Workflow for Branch", "sha", branch.Spec.SHA)

	branch.Status.Workflows = workflowNames
	branch.Status.ChangedFiles = changedFiles

	r.setCondition(&branch, "WorkflowReady", metav1.ConditionTrue, "WorkflowCreated", fmt.Sprintf("Workflow created successfully for SHA %s", branch.Spec.SHA))

	// Update annotation with the current SHA before status update
	if branch.Annotations == nil {
		branch.Annotations = make(map[string]string)
	}
	branch.Annotations["terrakojo.io/last-sha"] = branch.Spec.SHA
	if err := r.Update(ctx, &branch); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.Status().Update(ctx, &branch); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// setCondition sets a condition on the Branch status
func (r *BranchReconciler) setCondition(branch *terrakojoiov1alpha1.Branch, conditionType string, status metav1.ConditionStatus, reason, message string) {
	// Initialize conditions slice if nil
	if branch.Status.Conditions == nil {
		branch.Status.Conditions = []metav1.Condition{}
	}

	now := metav1.NewTime(time.Now())

	// Find existing condition with the same type
	for i, condition := range branch.Status.Conditions {
		if condition.Type == conditionType {
			// Update existing condition
			branch.Status.Conditions[i].Status = status
			branch.Status.Conditions[i].Reason = reason
			branch.Status.Conditions[i].Message = message
			branch.Status.Conditions[i].LastTransitionTime = now
			return
		}
	}

	// Add new condition (new type)
	branch.Status.Conditions = append(branch.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	})
}

func indexByOwnerBranchUID(obj client.Object) []string {
	workflow := obj.(*terrakojoiov1alpha1.Workflow)
	for _, ref := range workflow.OwnerReferences {
		if ref.Controller != nil && *ref.Controller && ref.Kind == "Branch" {
			return []string{string(ref.UID)}
		}
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *BranchReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Index Workflow resources by their controlling Branch UID for efficient lookups/deletions
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&terrakojoiov1alpha1.Workflow{},
		"metadata.ownerReferences.uid",
		indexByOwnerBranchUID,
	); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&terrakojoiov1alpha1.Branch{}).
		Owns(&terrakojoiov1alpha1.Workflow{}).
		Named("branch").
		Complete(r)
}

func (r *BranchReconciler) createWorkflowForBranch(ctx context.Context, branch *terrakojoiov1alpha1.Branch, templateName, workflowName, path string) error {
	workflow := &terrakojoiov1alpha1.Workflow{
		ObjectMeta: ctrl.ObjectMeta{
			Name:      workflowName,
			Namespace: branch.Namespace,
			Labels: map[string]string{
				"terrakojo.io/owner-uid": string(branch.UID),
			},
		},
		Spec: terrakojoiov1alpha1.WorkflowSpec{
			Branch:     branch.Name,
			Owner:      branch.Spec.Owner,
			Repository: branch.Spec.Repository,
			SHA:        branch.Spec.SHA,
			Template:   templateName,
			Path:       path,
		},
	}
	if err := controllerutil.SetControllerReference(branch, workflow, r.Scheme); err != nil {
		return err
	}

	return r.Create(ctx, workflow)
}

func (r *BranchReconciler) deleteWorkflowsForBranch(ctx context.Context, branch *terrakojoiov1alpha1.Branch) error {
	var workflows terrakojoiov1alpha1.WorkflowList
	if err := r.List(ctx, &workflows,
		client.InNamespace(branch.Namespace),
		client.MatchingFields{"metadata.ownerReferences.uid": string(branch.UID)},
	); err != nil {
		return fmt.Errorf("failed to list workflows for branch: %w", err)
	}
	for _, workflow := range workflows.Items {
		if err := r.Delete(ctx, &workflow); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("failed to delete workflow %s: %w", workflow.Name, err)
			}
		}
	}

	branch.Status.Workflows = []string{}
	return nil
}

func matchTemplates(templates terrakojoiov1alpha1.WorkflowTemplateList, changedFiles []string) map[string][]string {
	groups := map[string][]string{}
	for _, t := range templates.Items {
		files := matchTemplate(t.Spec.Match, changedFiles)
		if len(files) > 0 {
			groups[t.Name] = files
		}
	}
	return groups
}

func matchTemplate(match terrakojoiov1alpha1.WorkflowMatch, changedFiles []string) []string {
	matchedFiles := []string{}
	for _, file := range changedFiles {
		for _, pattern := range match.Paths {
			if matched, _ := doublestar.Match(pattern, file); matched {
				matchedFiles = append(matchedFiles, file)
				break
			}
		}
	}
	return matchedFiles
}

func splitFolderLevel(files []string) []string {
	folders := []string{}
	folderSet := make(map[string]struct{})
	for _, file := range files {
		dir := path.Dir(file)
		if dir != "." && dir != "" {
			folderSet[dir] = struct{}{}
		}
	}
	for folder := range folderSet {
		folders = append(folders, folder)
	}
	return folders
}

// generateRandomString generates a random string of specified length
func generateRandomString(length int) (string, error) {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, length)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = charset[b[i]%byte(len(charset))]
	}
	return string(b), nil
}

// listWorkflowsForBranch returns workflows owned by the branch using the UID field index.
func (r *BranchReconciler) listWorkflowsForBranch(ctx context.Context, branch *terrakojoiov1alpha1.Branch) ([]terrakojoiov1alpha1.Workflow, error) {
	var workflows terrakojoiov1alpha1.WorkflowList
	if err := r.List(ctx, &workflows,
		client.InNamespace(branch.Namespace),
		client.MatchingFields{"metadata.ownerReferences.uid": string(branch.UID)},
	); err != nil {
		return nil, err
	}
	return workflows.Items, nil
}

// allWorkflowsCompleted returns true if every workflow is in a terminal phase.
func allWorkflowsCompleted(workflows []terrakojoiov1alpha1.Workflow) bool {
	if len(workflows) == 0 {
		return false
	}
	for _, wf := range workflows {
		switch wf.Status.Phase {
		case string(WorkflowPhaseSucceeded), string(WorkflowPhaseFailed), string(WorkflowPhaseCancelled):
			continue
		default:
			return false
		}
	}
	return true
}

// cleanupRepositoryStatus removes the branch entry from the owning Repository status to prevent re-creation.
func (r *BranchReconciler) cleanupRepositoryStatus(ctx context.Context, branch *terrakojoiov1alpha1.Branch) error {
	log := logf.FromContext(ctx)

	repos := &terrakojoiov1alpha1.RepositoryList{}
	if err := r.List(ctx, repos,
		client.InNamespace(branch.Namespace),
		client.MatchingLabels{
			"terrakojo.io/owner":     branch.Spec.Owner,
			"terrakojo.io/repo-name": branch.Spec.Repository,
		},
	); err != nil {
		return fmt.Errorf("failed to list repositories for branch cleanup: %w", err)
	}
	if len(repos.Items) == 0 {
		log.Info("No repository found for branch cleanup",
			"owner", branch.Spec.Owner,
			"repo", branch.Spec.Repository)
		return nil
	}

	// Use optimistic retry to avoid conflicts with webhook status updates.
	repoName := repos.Items[0].Name
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var repo terrakojoiov1alpha1.Repository
		if err := r.Get(ctx, client.ObjectKey{Name: repoName, Namespace: branch.Namespace}, &repo); err != nil {
			return fmt.Errorf("failed to re-fetch repository %s: %w", repoName, err)
		}

		changed := false

		if branch.Spec.Name == repo.Spec.DefaultBranch {
			// Default branch: manage only the commit queue.
			before := len(repo.Status.DefaultBranchCommits)
			repo.Status.DefaultBranchCommits = slices.DeleteFunc(repo.Status.DefaultBranchCommits, func(c terrakojoiov1alpha1.BranchInfo) bool {
				return c.SHA == branch.Spec.SHA
			})
			if len(repo.Status.DefaultBranchCommits) != before {
				changed = true
			}
		} else {
			// Non-default branches live in BranchList.
			before := len(repo.Status.BranchList)
			repo.Status.BranchList = slices.DeleteFunc(repo.Status.BranchList, func(b terrakojoiov1alpha1.BranchInfo) bool {
				return b.Ref == branch.Spec.Name && b.SHA == branch.Spec.SHA
			})
			if len(repo.Status.BranchList) != before {
				changed = true
			}
		}

		if !changed {
			return nil
		}

		if err := r.Status().Update(ctx, &repo); err != nil {
			return err
		}

		log.Info("Cleaned repository status after branch completion",
			"repository", repo.Name,
			"branchRef", branch.Spec.Name,
			"sha", branch.Spec.SHA)
		return nil
	})
}
