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
	"path/filepath"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	terrakojoiov1alpha1 "github.com/eeekcct/terrakojo/api/v1alpha1"
)

// BranchReconciler reconciles a Branch object
type BranchReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=terrakojo.io,resources=branches,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=terrakojo.io,resources=branches/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=terrakojo.io,resources=branches/finalizers,verbs=update

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
			log.Error(err, "unable to update Branch status after failing to list WorkflowTemplates")
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	matched := &terrakojoiov1alpha1.WorkflowTemplate{}

	for _, t := range templates.Items {
		if matchTemplate(t.Spec.Match, branch.Status.ChangedFiles) {
			matched = &t
			break
		}
	}

	if matched.Name == "" {
		log.Info("No matching WorkflowTemplate found for Branch", "branch", branch.Name)
		return ctrl.Result{}, nil
	}

	// For now, we make one workflow per branch
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

	// update Branch status to reflect the created Workflow
	branch.Status.Workflows = append(branch.Status.Workflows, wfName)

	// Set condition to indicate workflow was created successfully
	r.setCondition(&branch, "WorkflowReady", metav1.ConditionTrue, "WorkflowCreated", fmt.Sprintf("Workflow %s created successfully", wfName))

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
			matched, err := filepath.Match(pattern, file)
			if err == nil && matched {
				return true
			}
		}
	}
	return false
}
