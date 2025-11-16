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
)

// WorkflowReconciler reconciles a Workflow object
type WorkflowReconciler struct {
	client.Client
	Scheme *runtime.Scheme
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

	jobName := fmt.Sprintf("%s-job", workflow.Name)

	var job batchv1.Job
	err := r.Get(ctx, client.ObjectKey{Name: jobName, Namespace: workflow.Namespace}, &job)
	if err != nil && client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, err
	}
	if err == nil {
		// Job exists, update workflow status based on job status
		if err := r.updateWorkflowStatus(ctx, &workflow, &job); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	job = r.createJobFromTemplate(jobName, &template)

	if err := controllerutil.SetControllerReference(&workflow, &job, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.Create(ctx, &job); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("Created Job for Workflow", "jobName", job.Name, "workflowName", workflow.Name, "namespace", workflow.Namespace)

	workflow.Status.Jobs = append(workflow.Status.Jobs, job.Name)
	if err := r.updateWorkflowStatus(ctx, &workflow, &job); err != nil {
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

func (r *WorkflowReconciler) updateWorkflowStatus(ctx context.Context, workflow *terrakojoiov1alpha1.Workflow, job *batchv1.Job) error {
	var status string
	if job.Status.Succeeded > 0 {
		status = "Succeeded"
	} else if job.Status.Failed > 0 {
		status = "Failed"
	} else if job.Status.Active > 0 {
		status = "Running"
	} else {
		status = "Pending"
	}

	r.setCondition(workflow, "JobStatus", metav1.ConditionTrue, status, fmt.Sprintf("Job is in %s state", status))

	if err := r.Status().Update(ctx, workflow); err != nil {
		return err
	}
	return nil
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
