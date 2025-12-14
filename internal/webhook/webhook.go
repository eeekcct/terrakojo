package webhook

import (
	"context"
	"log"
	"net/http"
	"strings"

	"k8s.io/client-go/util/retry"
	"slices"

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
		log.Printf("Processing GitHub pull request event: action=%s, PR #%d, repo=%s, branch=%s, sha=%s",
			string(event.Action), event.PullRequest.Number, event.Repository.FullName, event.PullRequest.Head.Ref, event.PullRequest.Head.Sha)
		webhookInfo = ghpkg.ProcessPullRequestEvent(event)

	case github.PushPayload:
		branchName := strings.TrimPrefix(event.Ref, "refs/heads/")
		log.Printf("Processing GitHub push event: repo=%s, branch=%s, sha=%s",
			event.Repository.FullName, branchName, event.After)
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
		return nil
	}

	if len(repositories.Items) > 1 {
		log.Printf("Warning: Multiple Repositories found for owner=%s name=%s, using the first one", webhookInfo.Owner, webhookInfo.RepositoryName)
	}

	targetRepository := &repositories.Items[0]
	key := client.ObjectKey{Name: targetRepository.Name, Namespace: targetRepository.Namespace}

	// Use optimistic retry to avoid clobbering concurrent controller updates.
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var repo v1alpha1.Repository
		if err := h.client.Get(context.Background(), key, &repo); err != nil {
			return err
		}

		changed := false

		// Default branch queue: append if this SHA is not already present.
		if branchInfo.Ref == repo.Spec.DefaultBranch {
			before := len(repo.Status.DefaultBranchCommits)
			exists := false
			for _, c := range repo.Status.DefaultBranchCommits {
				if c.SHA == branchInfo.SHA {
					exists = true
					break
				}
			}
			if !exists {
				repo.Status.DefaultBranchCommits = append(repo.Status.DefaultBranchCommits, branchInfo)
			}
			if len(repo.Status.DefaultBranchCommits) != before {
				changed = true
			}
		}

		// BranchList: only track non-default branches; default branch is managed via DefaultBranchCommits queue.
		if branchInfo.Ref != repo.Spec.DefaultBranch {
			// Remove exact ref+SHA duplicate, then append (ensures uniqueness per commit).
			before := len(repo.Status.BranchList)
			repo.Status.BranchList = slices.DeleteFunc(repo.Status.BranchList, func(b v1alpha1.BranchInfo) bool {
				return b.Ref == branchInfo.Ref && b.SHA == branchInfo.SHA
			})
			repo.Status.BranchList = append(repo.Status.BranchList, branchInfo)
			if len(repo.Status.BranchList) != before || branchInfo.PRNumber != 0 {
				changed = true
			}
		}

		if !changed {
			return nil
		}

		repo.Status.Synced = true
		return h.client.Status().Update(context.Background(), &repo)
	})

	if err != nil {
		log.Printf("Failed to update Repository %s status: %v", targetRepository.Name, err)
		return err
	}

	log.Printf("Updated Repository %s (spec.owner=%s spec.name=%s) with branch info: %+v", targetRepository.Name, webhookInfo.Owner, webhookInfo.RepositoryName, branchInfo)
	return nil
}
