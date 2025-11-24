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

// +kubebuilder:rbac:groups=terrakojo.io,resources=branches,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=terrakojo.io,resources=branches/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=terrakojo.io,resources=branches/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=terrakojo.io,resources=repositories,verbs=get;list;watch

// +kubebuilder:rbac:groups=terrakojo.io,resources=workflowtemplates,verbs=get;list;watch

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

	lastSHA := branch.Annotations["terrakojo.io/last-sha"]
	if lastSHA == branch.Spec.SHA {
		// No changes in SHA, nothing to do
		return ctrl.Result{}, nil
	}

	// Get GitHub client using manager (required)
	if r.GitHubClientManager == nil {
		log.Error(nil, "GitHubClientManager not initialized")
		return ctrl.Result{}, fmt.Errorf("GitHubClientManager not initialized")
	}

	ghClient, err := r.GitHubClientManager.GetClientForBranch(ctx, &branch)
	if err != nil {
		log.Error(err, "Failed to create GitHub client for branch",
			"branch", branch.Name,
			"owner", branch.Spec.Owner,
			"repository", branch.Spec.Repository)
		return ctrl.Result{}, err
	}

	// Get branch information from GitHub and update status
	githubBranch, err := ghClient.GetBranch(branch.Spec.Owner, branch.Spec.Repository, branch.Spec.Name)
	if err != nil {
		log.Error(err, "unable to get branch info from GitHub")
		r.setCondition(&branch, "BranchInfoFailed", metav1.ConditionFalse, "GitHubApiFailed", fmt.Sprintf("Failed to get branch info: %v", err))
		if statusErr := r.Status().Update(ctx, &branch); statusErr != nil {
			log.Error(statusErr, "unable to update Branch status after GitHub API failure")
		}
		return ctrl.Result{RequeueAfter: time.Minute * 5}, nil // Retry after 5 minutes
	}

	// Update branch SHA in status if it has changed
	if githubBranch.Commit != nil && githubBranch.Commit.SHA != nil {
		currentSHA := *githubBranch.Commit.SHA
		if branch.Spec.SHA != currentSHA {
			branch.Spec.SHA = currentSHA
			log.Info("Branch SHA updated", "branch", branch.Name, "sha", currentSHA)
		}
	}

	// Set condition to indicate branch info was retrieved successfully
	r.setCondition(&branch, "BranchInfoReady", metav1.ConditionTrue, "BranchInfoRetrieved", "Branch information retrieved successfully from GitHub")

	changedFiles, err := ghClient.GetChangedFiles(branch.Spec.Owner, branch.Spec.Repository, branch.Spec.PRNumber)
	if err != nil {
		log.Error(err, "unable to get changed files from GitHub")
		return ctrl.Result{}, err
	}
	if len(changedFiles) == 0 {
		return ctrl.Result{}, nil
	}

	var templates terrakojoiov1alpha1.WorkflowTemplateList
	if err := r.List(ctx, &templates); err != nil {
		log.Error(err, "unable to list WorkflowTemplates")
		r.setCondition(
			&branch,
			"WorkflowCreateFailed",
			metav1.ConditionFalse,
			"TemplateListFailed",
			"Failed to list WorkflowTemplates",
		)
		if err := r.Status().Update(ctx, &branch); err != nil {
			log.Error(err, "unable to update Branch status after failing to list WorkflowTemplates", "branch", branch.Name, "namespace", branch.Namespace)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	matched := &terrakojoiov1alpha1.WorkflowTemplate{}
	for _, t := range templates.Items {
		if matchTemplate(t.Spec.Match, changedFiles) {
			matched = &t
			break
		}
	}

	if matched.Name == "" {
		log.Info("No matching WorkflowTemplate found for Branch", "branch", branch.Name, "namespace", branch.Namespace, "changedFiles", changedFiles)
		return ctrl.Result{}, nil
	}

	wfName := fmt.Sprintf("%s-workflow", branch.Name)
	if err := r.Get(ctx, client.ObjectKey{Name: wfName, Namespace: branch.Namespace}, &terrakojoiov1alpha1.Workflow{}); err == nil {
		// Workflow already exists, nothing to do
		return ctrl.Result{}, nil
	}

	// Create a new Workflow for this Branch
	workflow := &terrakojoiov1alpha1.Workflow{
		ObjectMeta: ctrl.ObjectMeta{
			Name:      wfName,
			Namespace: branch.Namespace,
		},
		Spec: terrakojoiov1alpha1.WorkflowSpec{
			BranchRef: branch.Name,
			Template:  matched.Name, // This could be dynamic based on branch or other criteria
		},
	}
	if err := controllerutil.SetControllerReference(&branch, workflow, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.Create(ctx, workflow); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("Created Workflow for Branch", "workflow", wfName, "branch", branch.Name, "namespace", branch.Namespace)

	// update Branch status to reflect the created Workflow
	branch.Status.Workflows = append(branch.Status.Workflows, wfName)
	branch.Status.ChangedFiles = changedFiles

	// Set condition to indicate workflow was created successfully
	r.setCondition(&branch, "WorkflowReady", metav1.ConditionTrue, "WorkflowCreated", fmt.Sprintf("Workflow %s created successfully", wfName))

	// Update annotation with the current SHA before status update
	if branch.Annotations == nil {
		branch.Annotations = make(map[string]string)
	}
	branch.Annotations["terrakojo.io/last-sha"] = branch.Spec.SHA

	// Update the Branch resource first to persist annotation changes
	if err := r.Update(ctx, &branch); err != nil {
		return ctrl.Result{}, err
	}

	// Then update status
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

// SetupWithManager sets up the controller with the Manager.
func (r *BranchReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&terrakojoiov1alpha1.Branch{}).
		Owns(&terrakojoiov1alpha1.Workflow{}).
		Named("branch").
		Complete(r)
}

func matchTemplate(match terrakojoiov1alpha1.WorkflowMatch, changedFiles []string) bool {
	for _, pattern := range match.Paths {
		for _, file := range changedFiles {
			if matched, _ := doublestar.Match(pattern, file); matched {
				return true
			}
		}
	}
	return false
}
