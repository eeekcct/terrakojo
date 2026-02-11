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

package v1alpha1

import (
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// WorkflowTemplateSpec defines the desired state of WorkflowTemplate
type WorkflowTemplateSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	// The following markers will use OpenAPI v3 schema to validate the value
	// More info: https://book.kubebuilder.io/reference/markers/crd-validation.html

	// +required
	DisplayName string `json:"displayName"`

	// +required
	Match WorkflowMatch `json:"match"`

	// DependsOnTemplates defines template-level dependencies within the same
	// owner/repository/branch/sha/target scope.
	// Workflows from this template wait while listed template workflows are
	// running, and fail when dependency workflows are missing or failed.
	// +optional
	// +listType=set
	DependsOnTemplates []string `json:"dependsOnTemplates,omitempty"`

	// +required
	Job batchv1.JobSpec `json:"job"`
}

type WorkflowMatch struct {
	// +required
	Paths []string `json:"paths"`

	// ExecutionUnit controls how workflows are generated from matched files.
	// - folder: one Workflow per matched folder (current default behavior)
	// - repository: one Workflow for the repository
	// - file: one Workflow per matched file
	// +optional
	// +kubebuilder:validation:Enum=folder;repository;file
	ExecutionUnit WorkflowExecutionUnit `json:"executionUnit,omitempty"`
}

type WorkflowExecutionUnit string

const (
	WorkflowExecutionUnitFolder     WorkflowExecutionUnit = "folder"
	WorkflowExecutionUnitRepository WorkflowExecutionUnit = "repository"
	WorkflowExecutionUnitFile       WorkflowExecutionUnit = "file"
)

// WorkflowTemplateStatus defines the observed state of WorkflowTemplate.
type WorkflowTemplateStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the WorkflowTemplate resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 57",message="metadata.name must be 57 characters or less so derived Workflow names (GenerateName + suffix) remain <= 63"
// +kubebuilder:printcolumn:name="DisplayName",type=string,JSONPath=`.spec.displayName`
// +kubebuilder:printcolumn:name="Paths",type=string,JSONPath=`.spec.match.paths`
// +kubebuilder:printcolumn:name="Unit",type=string,JSONPath=`.spec.match.executionUnit`,priority=1

// WorkflowTemplate is the Schema for the workflowtemplates API
type WorkflowTemplate struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of WorkflowTemplate
	// +required
	Spec WorkflowTemplateSpec `json:"spec"`

	// status defines the observed state of WorkflowTemplate
	// +optional
	Status WorkflowTemplateStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// WorkflowTemplateList contains a list of WorkflowTemplate
type WorkflowTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WorkflowTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&WorkflowTemplate{}, &WorkflowTemplateList{})
}
