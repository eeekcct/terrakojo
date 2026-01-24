package webhook

import (
	"context"
	"log"
	"net/http"
	"strings"

	"slices"

	"k8s.io/client-go/util/retry"

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

const prActionClosed = "closed"

// NewHandler creates a new platform-agnostic webhook handler
func NewHandler(cfg *config.Config, kubeClient client.Client) (*Handler, error) {
	// Initialize GitHub webhook
	githubWebhook, err := github.New(github.Options.Secret(cfg.WebhookGithubSecret))
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
			event.Action, event.PullRequest.Number, event.Repository.FullName, event.PullRequest.Head.Ref, event.PullRequest.Head.Sha)
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
	if _, err := w.Write([]byte("OK")); err != nil {
		log.Printf("Failed to write webhook response: %v", err)
	}

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
	case ghpkg.EventTypePush:
		branchInfo := &v1alpha1.BranchInfo{
			Ref: webhookInfo.BranchName,
			SHA: webhookInfo.CommitSHA,
		}
		return branchInfo

	case ghpkg.EventTypePR:
		// If merged, treat as default-branch commit using merge_commit_sha and base ref
		if webhookInfo.Merged {
			return &v1alpha1.BranchInfo{
				Ref: webhookInfo.BaseBranchName,
				SHA: webhookInfo.MergeCommitSHA,
			}
		}

		// Otherwise track PR head
		branchInfo := &v1alpha1.BranchInfo{
			Ref: webhookInfo.BranchName,
			SHA: webhookInfo.CommitSHA,
		}
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

// updateBranchListWithLatestSHA updates the BranchList by replacing the entry for the given ref with the new SHA.
// Returns the updated BranchList and true if the BranchList was modified.
func updateBranchListWithLatestSHA(branchList []v1alpha1.BranchInfo, branchInfo v1alpha1.BranchInfo) ([]v1alpha1.BranchInfo, bool) {
	before := len(branchList)
	oldSHA := ""
	branchList = slices.DeleteFunc(branchList, func(b v1alpha1.BranchInfo) bool {
		if b.Ref == branchInfo.Ref {
			oldSHA = b.SHA
			return true
		}
		return false
	})
	branchList = append(branchList, branchInfo)
	changed := len(branchList) != before || oldSHA != branchInfo.SHA
	return branchList, changed
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

		// Default branch (push/merge): enqueue commit to defaultBranchCommits.
		if (webhookInfo.EventType == ghpkg.EventTypePush ||
			(webhookInfo.EventType == ghpkg.EventTypePR && webhookInfo.Action == prActionClosed && webhookInfo.Merged)) &&
			branchInfo.Ref == repo.Spec.DefaultBranch {
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

		// Push to non-default branch: keep latest SHA per ref in branchList.
		if webhookInfo.EventType == ghpkg.EventTypePush && branchInfo.Ref != repo.Spec.DefaultBranch {
			var branchChanged bool
			repo.Status.BranchList, branchChanged = updateBranchListWithLatestSHA(repo.Status.BranchList, branchInfo)
			if branchChanged {
				changed = true
			}
		}

		// PR open/synchronize: replace latest SHA for non-default branch.
		if webhookInfo.EventType == ghpkg.EventTypePR && webhookInfo.Action != prActionClosed {
			// Only update BranchList for non-default branches
			if branchInfo.Ref != repo.Spec.DefaultBranch {
				var branchChanged bool
				repo.Status.BranchList, branchChanged = updateBranchListWithLatestSHA(repo.Status.BranchList, branchInfo)
				if branchChanged {
					changed = true
				}
			}
		}

		// PR closed (merged/non-merged): remove PR head branch entry from list; leave defaultBranchCommits untouched.
		if webhookInfo.EventType == ghpkg.EventTypePR && webhookInfo.Action == prActionClosed {
			prHeadRef := webhookInfo.BranchName
			// Do not attempt to remove the default branch from BranchList
			if prHeadRef != repo.Spec.DefaultBranch {
				before := len(repo.Status.BranchList)
				repo.Status.BranchList = slices.DeleteFunc(repo.Status.BranchList, func(b v1alpha1.BranchInfo) bool {
					return b.Ref == prHeadRef
				})
				if len(repo.Status.BranchList) != before {
					changed = true
				}
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
