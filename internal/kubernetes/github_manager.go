package kubernetes

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/eeekcct/terrakojo/api/v1alpha1"
	"github.com/eeekcct/terrakojo/internal/github"
)

// GitHubCredentials contains GitHub authentication credentials
type GitHubCredentials struct {
	Type         github.GitHubAuthType
	Token        string
	AppID        int64
	Installation int64
	PrivateKey   string
}

type GitHubClientManagerInterface interface {
	GetClientForRepository(ctx context.Context, repo *v1alpha1.Repository) (github.ClientInterface, error)
	GetClientForBranch(ctx context.Context, branch *v1alpha1.Branch) (github.ClientInterface, error)
}

// GitHubClientManager manages GitHub client creation with Kubernetes secret integration
type GitHubClientManager struct {
	client.Client
}

// NewGitHubClientManager creates a new GitHub client manager
func NewGitHubClientManager(k8sClient client.Client) *GitHubClientManager {
	return &GitHubClientManager{
		Client: k8sClient,
	}
}

// GetClientForRepository gets a GitHub client for the specific repository
func (m *GitHubClientManager) GetClientForRepository(ctx context.Context, repo *v1alpha1.Repository) (github.ClientInterface, error) {

	// Get credentials from secret
	creds, err := m.getCredentialsFromSecret(ctx, &repo.Spec.GitHubSecretRef, repo.Namespace)
	if err != nil {
		return nil, fmt.Errorf("failed to get repository credentials: %w", err)
	}

	return github.NewClientFromCredentials(ctx, m.convertToGitHubCredentials(creds))
}

// GetClientForBranch gets a GitHub client for the branch by finding its parent repository
func (m *GitHubClientManager) GetClientForBranch(ctx context.Context, branch *v1alpha1.Branch) (github.ClientInterface, error) {

	// Find the parent repository
	repo, err := m.findRepositoryForBranch(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("failed to find repository for branch: %w", err)
	}

	return m.GetClientForRepository(ctx, repo)
}

// convertToGitHubCredentials converts kubernetes GitHubCredentials to github package GitHubCredentials
func (m *GitHubClientManager) convertToGitHubCredentials(creds *GitHubCredentials) *github.GitHubCredentials {
	return &github.GitHubCredentials{
		Type:         creds.Type,
		Token:        creds.Token,
		AppID:        creds.AppID,
		Installation: creds.Installation,
		PrivateKey:   creds.PrivateKey,
	}
}
