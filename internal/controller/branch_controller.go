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
	"path"
	"strconv"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

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
//
//nolint:gocyclo // Reconcile handles multiple lifecycle branches for clarity.
func (r *BranchReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var branch terrakojoiov1alpha1.Branch
	if err := r.Get(ctx, req.NamespacedName, &branch); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion: ensure workflows are removed before allowing branch GC
	if !branch.DeletionTimestamp.IsZero() {
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

	repo, err := r.getRepositoryForBranch(ctx, &branch)
	if err != nil {
		log.Error(err, "Failed to get repository for branch")
		return ctrl.Result{}, err
	}
	if repo == nil {
		log.Info("Repository not found for branch; deleting branch", "branch", branch.Spec.Name, "sha", branch.Spec.SHA)
		if err := r.Delete(ctx, &branch); err != nil && client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}
	isDefaultBranch := branch.Spec.Name == repo.Spec.DefaultBranch

	lastSHA := branch.Annotations["terrakojo.io/last-sha"]

	// List workflows owned by this branch to decide whether to keep or delete.
	workflows, err := r.listWorkflowsForBranch(ctx, &branch)
	if err != nil {
		log.Error(err, "Failed to list workflows for branch")
		return ctrl.Result{}, err
	}

	// If the SHA hasn't changed, either delete (default branch) or keep based on workflows.
	if lastSHA == branch.Spec.SHA {
		if len(workflows) == 0 {
			if isDefaultBranch {
				if err := r.Delete(ctx, &branch); err != nil && client.IgnoreNotFound(err) != nil {
					log.Error(err, "Failed to delete branch with no workflows")
					return ctrl.Result{}, err
				}
				log.Info("Deleted Branch for default branch commit with no workflows",
					"branchName", branch.Spec.Name,
					"sha", branch.Spec.SHA)
			}
			return ctrl.Result{}, nil
		}

		if allWorkflowsCompleted(workflows) {
			if isDefaultBranch {
				if err := r.Delete(ctx, &branch); err != nil && client.IgnoreNotFound(err) != nil {
					log.Error(err, "Failed to delete completed branch")
					return ctrl.Result{}, err
				}
				log.Info("Deleted Branch after all workflows completed",
					"branchName", branch.Spec.Name,
					"sha", branch.Spec.SHA)
			} else {
				log.Info("All workflows completed; keeping Branch",
					"branchName", branch.Spec.Name,
					"sha", branch.Spec.SHA)
			}
			return ctrl.Result{}, nil
		}

		// Workflows still running; nothing to do.
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

	var changedFiles []string
	if branch.Spec.PRNumber != 0 {
		// PR branch: use PR API to get changed files
		changedFiles, err = ghClient.GetChangedFiles(branch.Spec.Owner, branch.Spec.Repository, branch.Spec.PRNumber)
		if err != nil {
			log.Error(err, "unable to get changed files from GitHub PR")
			return ctrl.Result{}, err
		}
	} else {
		// Default branch or direct push: use commit API to get changed files
		changedFiles, err = ghClient.GetChangedFilesForCommit(branch.Spec.Owner, branch.Spec.Repository, branch.Spec.SHA)
		if err != nil {
			log.Error(err, "unable to get changed files from GitHub commit")
			return ctrl.Result{}, err
		}
	}
	if len(changedFiles) == 0 {
		log.Info("No changed files for Branch")
		if isDefaultBranch {
			if err := r.Delete(ctx, &branch); err != nil && client.IgnoreNotFound(err) != nil {
				log.Error(err, "Failed to delete branch with no changed files")
				return ctrl.Result{}, err
			}
		} else if err := r.markBranchSHA(ctx, &branch); err != nil {
			return ctrl.Result{}, err
		}
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
		log.Info("No matching WorkflowTemplate found for Branch")
		if isDefaultBranch {
			if err := r.Delete(ctx, &branch); err != nil && client.IgnoreNotFound(err) != nil {
				log.Error(err, "Failed to delete branch with no matching templates")
				return ctrl.Result{}, err
			}
		} else if err := r.markBranchSHA(ctx, &branch); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	workflowNames := []string{}
	for templateName, files := range groups {
		folders := splitFolderLevel(files)
		if len(folders) == 0 {
			continue
		}
		for _, folder := range folders {
			createdName, err := r.createWorkflowForBranch(ctx, &branch, templateName, fmt.Sprintf("%s-", templateName), folder, isDefaultBranch)
			if err != nil {
				log.Error(err, "Failed to create Workflow for Branch")
				return ctrl.Result{}, err
			}
			workflowNames = append(workflowNames, createdName)
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

func (r *BranchReconciler) createWorkflowForBranch(
	ctx context.Context,
	branch *terrakojoiov1alpha1.Branch,
	templateName,
	workflowGenerateName,
	workflowPath string,
	isDefaultBranch bool,
) (string, error) {
	workflow := &terrakojoiov1alpha1.Workflow{
		ObjectMeta: ctrl.ObjectMeta{
			GenerateName: workflowGenerateName,
			Namespace:    branch.Namespace,
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
			Path:       workflowPath,
			Parameters: map[string]string{
				"isDefaultBranch": strconv.FormatBool(isDefaultBranch),
			},
		},
	}
	if err := controllerutil.SetControllerReference(branch, workflow, r.Scheme); err != nil {
		return "", err
	}

	if err := r.Create(ctx, workflow); err != nil {
		return "", err
	}

	return workflow.Name, nil
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

func (r *BranchReconciler) markBranchSHA(ctx context.Context, branch *terrakojoiov1alpha1.Branch) error {
	if branch.Annotations == nil {
		branch.Annotations = map[string]string{}
	}
	if branch.Annotations["terrakojo.io/last-sha"] == branch.Spec.SHA {
		return nil
	}
	branch.Annotations["terrakojo.io/last-sha"] = branch.Spec.SHA
	return r.Update(ctx, branch)
}

func (r *BranchReconciler) getRepositoryForBranch(ctx context.Context, branch *terrakojoiov1alpha1.Branch) (*terrakojoiov1alpha1.Repository, error) {
	for _, ref := range branch.GetOwnerReferences() {
		if ref.Kind != "Repository" || ref.Name == "" {
			continue
		}
		var repo terrakojoiov1alpha1.Repository
		if err := r.Get(ctx, client.ObjectKey{Name: ref.Name, Namespace: branch.Namespace}, &repo); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return nil, err
			}
			break
		}
		return &repo, nil
	}
	return nil, nil
}
