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
	"regexp"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	terrakojoiov1alpha1 "github.com/eeekcct/terrakojo/api/v1alpha1"
	"github.com/eeekcct/terrakojo/internal/kubernetes"
)

// WorkflowPhase represents the phase of a workflow
type WorkflowPhase string

const (
	// WorkflowPhasePending indicates the workflow is waiting to start
	WorkflowPhasePending WorkflowPhase = "Pending"

	// WorkflowPhaseRunning indicates the workflow is currently running
	WorkflowPhaseRunning WorkflowPhase = "Running"

	// WorkflowPhaseSucceeded indicates the workflow completed successfully
	WorkflowPhaseSucceeded WorkflowPhase = "Succeeded"

	// WorkflowPhaseFailed indicates the workflow failed
	WorkflowPhaseFailed WorkflowPhase = "Failed"

	// WorkflowPhaseCancelled indicates the workflow was cancelled
	WorkflowPhaseCancelled WorkflowPhase = "Cancelled"
)

// WorkflowReconciler reconciles a Workflow object
type WorkflowReconciler struct {
	client.Client
	Scheme              *runtime.Scheme
	GitHubClientManager kubernetes.GitHubClientManagerInterface
}

// +kubebuilder:rbac:groups=terrakojo.io,resources=workflows,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=terrakojo.io,resources=workflows/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=terrakojo.io,resources=workflows/finalizers,verbs=update

// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Workflow object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.1/pkg/reconcile
func (r *WorkflowReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var workflow terrakojoiov1alpha1.Workflow
	if err := r.Get(ctx, req.NamespacedName, &workflow); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var template terrakojoiov1alpha1.WorkflowTemplate
	if err := r.Get(ctx, client.ObjectKey{Name: workflow.Spec.Template, Namespace: workflow.Namespace}, &template); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var branch terrakojoiov1alpha1.Branch
	if err := r.Get(ctx, client.ObjectKey{Name: workflow.Spec.BranchRef, Namespace: workflow.Namespace}, &branch); err != nil {
		log.Error(err, "Failed to get Branch for Workflow",
			"workflow", workflow.Name,
			"branchRef", workflow.Spec.BranchRef)
		return ctrl.Result{}, err
	}

	if r.GitHubClientManager == nil {
		log.Error(nil, "GitHubClientManager not initialized")
		return ctrl.Result{}, fmt.Errorf("GitHubClientManager not initialized")
	}
	ghClient, err := r.GitHubClientManager.GetClientForBranch(ctx, &branch)
	if err != nil {
		log.Error(err, "Failed to create GitHub client for workflow",
			"workflow", workflow.Name,
			"branch", branch.Name,
			"owner", branch.Spec.Owner,
			"repository", branch.Spec.Repository)
		return ctrl.Result{}, err
	}

	checkRunName := fmt.Sprintf("terrakojo(%s)", workflow.Spec.Path)
	jobName := fmt.Sprintf("%s-job", workflow.Name)
	var job batchv1.Job
	err = r.Get(ctx, client.ObjectKey{Name: jobName, Namespace: workflow.Namespace}, &job)
	if err != nil && client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, err
	}
	if err == nil {
		// Job exists, update workflow status based on job status
		phase, err := r.updateWorkflowStatus(ctx, &workflow, &job)
		if err != nil {
			return ctrl.Result{}, err
		}
		status, conclusion := r.checkRunStatus(ctx, &workflow, phase)
		err = ghClient.UpdateCheckRun(branch.Spec.Owner, branch.Spec.Repository, int64(workflow.Status.CheckRunID), checkRunName, status, conclusion)
		if err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Create CheckRun
	checkRun, err := ghClient.CreateCheckRun(branch.Spec.Owner, branch.Spec.Repository, branch.Spec.SHA, checkRunName)
	if err != nil {
		log.Error(err, "Failed to create GitHub CheckRun for workflow",
			"workflow", workflow.Name,
			"branch", branch.Name,
			"owner", branch.Spec.Owner,
			"repository", branch.Spec.Repository)
		return ctrl.Result{}, err
	}
	workflow.Status.CheckRunID = int(checkRun.GetID())
	if err := r.Status().Update(ctx, &workflow); err != nil {
		if client.IgnoreNotFound(err) == nil {
			// Workflow was deleted, ignore this reconcile
			log.Info("Workflow was deleted during reconcile, ignoring",
				"workflow", workflow.Name,
				"checkRunID", workflow.Status.CheckRunID)
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to update Workflow status with CheckRunID",
			"workflow", workflow.Name,
			"checkRunID", workflow.Status.CheckRunID)
		return ctrl.Result{}, err
	}

	// Create Job from WorkflowTemplate
	job = r.createJobFromTemplate(jobName, &template)

	if err := controllerutil.SetControllerReference(&workflow, &job, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.Create(ctx, &job); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("Created Job for Workflow", "jobName", job.Name, "workflowName", workflow.Name, "namespace", workflow.Namespace)

	workflow.Status.Jobs = append(workflow.Status.Jobs, job.Name)
	phase, err := r.updateWorkflowStatus(ctx, &workflow, &job)
	if err != nil {
		return ctrl.Result{}, err
	}
	status, conclusion := r.checkRunStatus(ctx, &workflow, phase)
	err = ghClient.UpdateCheckRun(branch.Spec.Owner, branch.Spec.Repository, int64(workflow.Status.CheckRunID), checkRunName, status, conclusion)
	if err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkflowReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&terrakojoiov1alpha1.Workflow{}).
		Owns(&batchv1.Job{}).
		Named("workflow").
		Complete(r)
}

func (r *WorkflowReconciler) createJobFromTemplate(jobName string, template *terrakojoiov1alpha1.WorkflowTemplate) batchv1.Job {
	// for now, we execute first step only
	step := template.Spec.Steps[0]

	// Normalize container name to comply with RFC 1123
	containerName := r.normalizeContainerName(step.Name)

	return batchv1.Job{
		ObjectMeta: ctrl.ObjectMeta{
			Name:      jobName,
			Namespace: template.Namespace,
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    containerName,
							Image:   step.Image,
							Command: step.Command,
						},
					},
				},
			},
		},
	}
}

// normalizeContainerName converts a name to a valid Kubernetes container name
// RFC 1123 compliance: lowercase alphanumeric characters or '-', start and end with alphanumeric
func (r *WorkflowReconciler) normalizeContainerName(name string) string {
	// Convert to lowercase
	normalized := strings.ToLower(name)

	// Replace spaces and invalid characters with hyphens
	normalized = regexp.MustCompile(`[^a-z0-9\-]+`).ReplaceAllString(normalized, "-")

	// Remove leading/trailing hyphens
	normalized = strings.Trim(normalized, "-")

	// Ensure it starts with alphanumeric
	if len(normalized) == 0 {
		normalized = "step"
	} else if !regexp.MustCompile(`^[a-z0-9]`).MatchString(normalized) {
		normalized = "step-" + normalized
	}

	// Ensure it ends with alphanumeric
	if !regexp.MustCompile(`[a-z0-9]$`).MatchString(normalized) {
		normalized = normalized + "-step"
	}

	return normalized
}

func (r *WorkflowReconciler) updateWorkflowStatus(ctx context.Context, workflow *terrakojoiov1alpha1.Workflow, job *batchv1.Job) (WorkflowPhase, error) {
	var phase WorkflowPhase
	if job.Status.Succeeded > 0 {
		phase = WorkflowPhaseSucceeded
	} else if job.Status.Failed > 0 {
		phase = WorkflowPhaseFailed
	} else if job.Status.Active > 0 {
		phase = WorkflowPhaseRunning
	} else {
		phase = WorkflowPhasePending
	}

	r.setCondition(workflow, "JobStatus", metav1.ConditionTrue, string(phase), fmt.Sprintf("Job is in %s state", phase))

	if err := r.Status().Update(ctx, workflow); err != nil {
		if client.IgnoreNotFound(err) == nil {
			// Workflow was deleted, ignore this reconcile
			return phase, nil
		}
		return phase, err
	}
	return phase, nil
}

func (r *WorkflowReconciler) checkRunStatus(ctx context.Context, workflow *terrakojoiov1alpha1.Workflow, phase WorkflowPhase) (string, string) {
	var status, conclusion string
	switch phase {
	case WorkflowPhasePending:
		status = "queued"
		conclusion = ""
	case WorkflowPhaseRunning:
		status = "in_progress"
		conclusion = ""
	case WorkflowPhaseSucceeded:
		status = "completed"
		conclusion = "success"
	case WorkflowPhaseFailed:
		status = "completed"
		conclusion = "failure"
	case WorkflowPhaseCancelled:
		status = "completed"
		conclusion = "cancelled"
	default:
		status = "queued"
		conclusion = ""
	}
	return status, conclusion
}

func (r *WorkflowReconciler) setCondition(workflow *terrakojoiov1alpha1.Workflow, conditionType string, status metav1.ConditionStatus, reason, message string) {
	if workflow.Status.Conditions == nil {
		workflow.Status.Conditions = []metav1.Condition{}
	}

	now := metav1.NewTime(time.Now())

	// Find existing condition with the same type
	for i, cond := range workflow.Status.Conditions {
		if cond.Type == conditionType {
			workflow.Status.Conditions[i].Status = status
			workflow.Status.Conditions[i].Reason = reason
			workflow.Status.Conditions[i].Message = message
			workflow.Status.Conditions[i].LastTransitionTime = now
			return
		}
	}

	// Add new condition if not found
	workflow.Status.Conditions = append(workflow.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	})
}
