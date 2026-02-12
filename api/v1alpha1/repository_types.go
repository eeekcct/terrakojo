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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// BranchInfo represents a branch within a repository
type BranchInfo struct {
	// Ref is the branch name (e.g., "main", "feature/test")
	// +required
	Ref string `json:"ref"`

	// PRNumber is the pull request number associated with this branch (if any)
	// +optional
	PRNumber int `json:"prNumber,omitempty"`

	// SHA is the commit SHA of the branch head
	// +optional
	SHA string `json:"sha,omitempty"`
}

// GitHubSecretRef references a Secret containing GitHub credentials
type GitHubSecretRef struct {
	// Name of the secret containing GitHub credentials
	// +required
	Name string `json:"name"`

	// Namespace of the secret. If empty, uses the same namespace as Repository
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// RepositorySpec defines the desired state of Repository
type RepositorySpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	// The following markers will use OpenAPI v3 schema to validate the value
	// More info: https://book.kubebuilder.io/reference/markers/crd-validation.html

	// +required
	Owner string `json:"owner"`

	// +required
	Name string `json:"name"`

	// +required
	Type string `json:"type"`

	// +required
	DefaultBranch string `json:"defaultBranch"`

	// GitHubSecretRef references a secret containing GitHub authentication credentials.
	// This field is required to ensure proper authentication for GitHub operations.
	// +required
	GitHubSecretRef GitHubSecretRef `json:"githubSecretRef"`
}

// RepositoryStatus defines the observed state of Repository.
type RepositoryStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the Repository resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	// Terrakojo-specific condition types include:
	// - "BootstrapReady": default-branch cutover point is initialized from current HEAD
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// LastDefaultBranchHeadSHA stores the default branch head SHA observed during the last GitHub sync.
	// +optional
	LastDefaultBranchHeadSHA string `json:"lastDefaultBranchHeadSha,omitempty"`

	// Synced indicates whether the repository has been successfully synced
	// +optional
	Synced bool `json:"synced,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Owner",type=string,JSONPath=`.spec.owner`
// +kubebuilder:printcolumn:name="Repo",type=string,JSONPath=`.spec.name`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`,priority=1
// +kubebuilder:printcolumn:name="Default",type=string,JSONPath=`.spec.defaultBranch`,priority=1
// +kubebuilder:printcolumn:name="Synced",type=boolean,JSONPath=`.status.synced`,priority=1
// +kubebuilder:printcolumn:name="LastHead",type=string,JSONPath=`.status.lastDefaultBranchHeadSha`

// Repository is the Schema for the repositories API
type Repository struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of Repository
	// +required
	Spec RepositorySpec `json:"spec"`

	// status defines the observed state of Repository
	// +optional
	Status RepositoryStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// RepositoryList contains a list of Repository
type RepositoryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Repository `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Repository{}, &RepositoryList{})
}
