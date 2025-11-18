package webhook

import (
	"context"
	"log"
	"net/http"

	"github.com/eeekcct/terrakojo/api/v1alpha1"
	"github.com/eeekcct/terrakojo/internal/config"
	ghpkg "github.com/eeekcct/terrakojo/internal/github"
	"github.com/go-playground/webhooks/v6/github"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Handler handles webhook events from various platforms
type Handler struct {
	githubWebhook *github.Webhook
	client        client.Client
}

// NewHandler creates a new platform-agnostic webhook handler
func NewHandler(config *config.Config, kubeClient client.Client) (*Handler, error) {
	// Initialize GitHub webhook
	githubWebhook, err := github.New(github.Options.Secret(config.WebhookGithubSecret))
	if err != nil {
		return nil, err
	}

	return &Handler{
		githubWebhook: githubWebhook,
		client:        kubeClient,
	}, nil
}

// ServeHTTP implements http.Handler interface
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Determine the webhook source
	switch {
	case r.Header.Get("X-GitHub-Event") != "":
		h.handleGitHubWebhook(w, r)
	case r.Header.Get("X-GitLab-Event") != "":
		// TODO: Handle GitLab webhooks
		log.Printf("GitLab webhook received (not yet implemented)")
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	default:
		log.Printf("Unknown webhook source")
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

// handleGitHubWebhook processes GitHub webhook events
func (h *Handler) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	// Parse the webhook payload
	payload, err := h.githubWebhook.Parse(r, github.PushEvent, github.PullRequestEvent, github.PingEvent)
	if err != nil {
		log.Printf("Error parsing GitHub webhook: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	var webhookInfo *ghpkg.WebhookInfo

	// Process the payload based on its type
	switch event := payload.(type) {
	case github.PullRequestPayload:
		webhookInfo = ghpkg.ProcessPullRequestEvent(event)

	case github.PushPayload:
		webhookInfo = ghpkg.ProcessPushEvent(event)

	case github.PingPayload:
		log.Printf("Received GitHub ping event")
		webhookInfo = &ghpkg.WebhookInfo{
			EventType: ghpkg.EventTypePing,
		}

	default:
		log.Printf("Unhandled GitHub event type: %T", event)
		webhookInfo = nil
	}

	// Respond to GitHub immediately
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))

	if webhookInfo == nil {
		return
	}

	// Convert WebhookInfo to BranchInfo for repository updates
	branchInfo := h.convertWebhookInfoToBranchInfo(webhookInfo)
	if branchInfo == nil {
		return
	}

	if err := h.updateRepositoryBranchList(*webhookInfo, *branchInfo); err != nil {
		log.Printf("Error updating repository: %v", err)
	}
}

// convertWebhookInfoToBranchInfo converts generic webhook info to BranchInfo
// Returns nil for events that don't require repository updates (like ping events)
func (h *Handler) convertWebhookInfoToBranchInfo(webhookInfo *ghpkg.WebhookInfo) *v1alpha1.BranchInfo {
	// Only process push and pull request events
	switch webhookInfo.EventType {
	case ghpkg.EventTypePush, ghpkg.EventTypePR:
		branchInfo := &v1alpha1.BranchInfo{
			Ref: webhookInfo.BranchName,
			SHA: webhookInfo.CommitSHA,
		}

		// Set PR number if applicable
		if webhookInfo.PRNumber != nil {
			branchInfo.PRNumber = *webhookInfo.PRNumber
		}

		return branchInfo

	case ghpkg.EventTypePing:
		return nil

	default:
		log.Printf("Unhandled webhook event type: %s", webhookInfo.EventType)
		return nil
	}
}

// updateRepositoryBranchList updates the Repository's BranchList with the new branch information
func (h *Handler) updateRepositoryBranchList(webhookInfo ghpkg.WebhookInfo, branchInfo v1alpha1.BranchInfo) error {
	// Use label selector to efficiently find the Repository
	repositories := &v1alpha1.RepositoryList{}
	labelSelector := labels.SelectorFromSet(map[string]string{
		"terrakojo.io/owner":     webhookInfo.Owner,
		"terrakojo.io/repo-name": webhookInfo.RepositoryName,
	})

	err := h.client.List(context.Background(), repositories, &client.ListOptions{
		LabelSelector: labelSelector,
	})

	if err != nil {
		log.Printf("Failed to list Repositories with labels: %v", err)
		return err
	}

	if len(repositories.Items) == 0 {
		log.Printf("Repository with spec.owner=%s spec.name=%s not found", webhookInfo.Owner, webhookInfo.RepositoryName)
		return nil // Don't treat as error, just skip
	}

	if len(repositories.Items) > 1 {
		log.Printf("Warning: Multiple Repositories found for owner=%s name=%s, using the first one", webhookInfo.Owner, webhookInfo.RepositoryName)
	}

	targetRepository := &repositories.Items[0]

	// Update or add the branch in the BranchList
	updated := false
	for i, existingBranch := range targetRepository.Status.BranchList {
		if existingBranch.Ref == branchInfo.Ref {
			targetRepository.Status.BranchList[i] = branchInfo
			updated = true
			break
		}
	}

	if !updated {
		targetRepository.Status.BranchList = append(targetRepository.Status.BranchList, branchInfo)
	}

	// Mark as synced
	targetRepository.Status.Synced = true

	// Update the Repository status
	err = h.client.Status().Update(context.Background(), targetRepository)
	if err != nil {
		log.Printf("Failed to update Repository %s status: %v", targetRepository.Name, err)
		return err
	}

	log.Printf("Updated Repository %s (spec.owner=%s spec.name=%s) with branch info: %+v", targetRepository.Name, webhookInfo.Owner, webhookInfo.RepositoryName, branchInfo)
	return nil
}
