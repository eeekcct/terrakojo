package webhook

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

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

const syncRequestAnnotation = "terrakojo.io/sync-requested-at"

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

	if webhookInfo.EventType == ghpkg.EventTypePing {
		return
	}

	if err := h.requestRepositorySync(*webhookInfo); err != nil {
		log.Printf("Error requesting repository sync: %v", err)
	}
}

// requestRepositorySync updates a Repository annotation to trigger a reconcile.
func (h *Handler) requestRepositorySync(webhookInfo ghpkg.WebhookInfo) error {
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

		if repo.Annotations == nil {
			repo.Annotations = map[string]string{}
		}
		// Set sync-requested-at annotation to trigger an immediate reconcile.
		// The annotation value itself is not used by the controller; it only
		// serves as a trigger for Kubernetes to invoke the Repository reconciler,
		// which then performs a full sync from GitHub.
		repo.Annotations[syncRequestAnnotation] = time.Now().UTC().Format(time.RFC3339Nano)
		return h.client.Update(context.Background(), &repo)
	})

	if err != nil {
		log.Printf("Failed to update Repository %s: %v", targetRepository.Name, err)
		return err
	}

	log.Printf("Requested Repository sync for %s (spec.owner=%s spec.name=%s)", targetRepository.Name, webhookInfo.Owner, webhookInfo.RepositoryName)
	return nil
}
