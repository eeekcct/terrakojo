package kubernetes

import (
	"context"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/eeekcct/terrakojo/api/v1alpha1"
	"github.com/eeekcct/terrakojo/internal/github"
)

// getCredentialsFromSecret extracts GitHub credentials from a Kubernetes secret
func (m *GitHubClientManager) getCredentialsFromSecret(ctx context.Context, secretRef *v1alpha1.GitHubSecretRef, defaultNamespace string) (*GitHubCredentials, error) {
	namespace := secretRef.Namespace
	if namespace == "" {
		namespace = defaultNamespace
	}

	secret := &corev1.Secret{}
	err := m.Get(ctx, client.ObjectKey{
		Name:      secretRef.Name,
		Namespace: namespace,
	}, secret)
	if err != nil {
		return nil, fmt.Errorf("failed to get secret %s/%s: %w", namespace, secretRef.Name, err)
	}

	return parseCredentialsFromSecret(secret)
}

// parseCredentialsFromSecret parses GitHub credentials from secret data
func parseCredentialsFromSecret(secret *corev1.Secret) (*GitHubCredentials, error) {
	// Check for token-based authentication
	if token, exists := secret.Data["token"]; exists && len(token) > 0 {
		return &GitHubCredentials{
			Type:  github.GitHubAuthTypeToken,
			Token: string(token),
		}, nil
	}

	// Check for GitHub App authentication
	if appIDData, exists := secret.Data["github-app-id"]; exists {
		appID, err := strconv.ParseInt(string(appIDData), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid github-app-id: %w", err)
		}

		installationIDData, exists := secret.Data["github-installation-id"]
		if !exists {
			return nil, fmt.Errorf("github-installation-id is required for GitHub App authentication")
		}
		installationID, err := strconv.ParseInt(string(installationIDData), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid github-installation-id: %w", err)
		}

		privateKeyData, exists := secret.Data["github-private-key"]
		if !exists {
			return nil, fmt.Errorf("github-private-key is required for GitHub App authentication")
		}

		return &GitHubCredentials{
			Type:         github.GitHubAuthTypeGitHubApp,
			AppID:        appID,
			Installation: installationID,
			PrivateKey:   string(privateKeyData),
		}, nil
	}

	return nil, fmt.Errorf("secret does not contain valid GitHub credentials (expected 'token' or GitHub App fields)")
}
