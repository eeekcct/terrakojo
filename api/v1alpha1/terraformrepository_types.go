/*
Copyright 2025.

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// TerraformRepositorySpec defines the desired state of TerraformRepository
type TerraformRepositorySpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	// The following markers will use OpenAPI v3 schema to validate the value
	// More info: https://book.kubebuilder.io/reference/markers/crd-validation.html

	// +required
	RepoURL string `json:"repoURL"`
	// +optional
	DefaultBranch string `json:"defaultBranch,omitempty"`
	// +optional
	Paths              []TerraformRepositoryPath `json:"paths,omitempty"`
	GithubAppSecretRef *SecretRef                `json:"githubAppSecretRef,omitempty"`
	MaxConcurrentPlans int                       `json:"maxConcurrentPlans,omitempty"`
}

// TerraformRepositoryPath defines a path within the repository to manage.
type TerraformRepositoryPath struct {
	Name  string `json:"name"`
	Order int    `json:"order,omitempty"`
}

type SecretRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// TerraformRepositoryStatus defines the observed state of TerraformRepository.
type TerraformRepositoryStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the TerraformRepository resource.
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
	Conditions     []metav1.Condition `json:"conditions,omitempty"`
	LastSync       *metav1.Time       `json:"lastSync,omitempty"`
	ActiveBranches []string           `json:"activeBranches,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// TerraformRepository is the Schema for the terraformrepositories API
type TerraformRepository struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of TerraformRepository
	// +required
	Spec TerraformRepositorySpec `json:"spec"`

	// status defines the observed state of TerraformRepository
	// +optional
	Status TerraformRepositoryStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// TerraformRepositoryList contains a list of TerraformRepository
type TerraformRepositoryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TerraformRepository `json:"items"`
}

func init() {
	SchemeBuilder.Register(&TerraformRepository{}, &TerraformRepositoryList{})
}
