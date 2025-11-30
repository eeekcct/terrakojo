package kubernetes

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/eeekcct/terrakojo/api/v1alpha1"
)

// findRepositoryForBranch finds the Repository resource for a given Branch
func (m *GitHubClientManager) findRepositoryForBranch(ctx context.Context, branch *v1alpha1.Branch) (*v1alpha1.Repository, error) {
	// First try to find via owner reference
	for _, ownerRef := range branch.GetOwnerReferences() {
		if ownerRef.Kind == "Repository" {
			repo := &v1alpha1.Repository{}
			err := m.Get(ctx, client.ObjectKey{
				Name:      ownerRef.Name,
				Namespace: branch.Namespace,
			}, repo)
			if err == nil {
				return repo, nil
			}
		}
	}

	// Fallback: search by owner and repository name
	repositories := &v1alpha1.RepositoryList{}
	err := m.List(ctx, repositories,
		client.InNamespace(branch.Namespace),
		client.MatchingLabels{
			"terrakojo.io/owner":     branch.Spec.Owner,
			"terrakojo.io/repo-name": branch.Spec.Repository,
		})
	if err != nil {
		return nil, fmt.Errorf("failed to list repositories: %w", err)
	}

	for _, repo := range repositories.Items {
		if repo.Spec.Owner == branch.Spec.Owner && repo.Spec.Name == branch.Spec.Repository {
			return &repo, nil
		}
	}

	return nil, fmt.Errorf("repository not found for owner=%s, name=%s",
		branch.Spec.Owner, branch.Spec.Repository)
}
