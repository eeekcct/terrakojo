package github

import (
	"log"
	"strings"

	"github.com/go-playground/webhooks/v6/github"
)

// WebhookEventType represents the type of webhook event
type WebhookEventType string

const (
	EventTypePush WebhookEventType = "push"
	EventTypePR   WebhookEventType = "pull_request"
	EventTypePing WebhookEventType = "ping"
)

// WebhookInfo contains generic webhook information
type WebhookInfo struct {
	EventType          WebhookEventType `json:"eventType"`
	Owner              string           `json:"owner"`
	RepositoryName     string           `json:"repositoryName"`
	RepositoryFullName string           `json:"repositoryFullName"`
	BranchName         string           `json:"branchName,omitempty"`
	CommitSHA          string           `json:"commitSha,omitempty"`
	PRNumber           *int             `json:"prNumber,omitempty"`
	Action             string           `json:"action,omitempty"` // for PR events: opened, closed, etc.
}

// ProcessPullRequestEvent extracts WebhookInfo from pull request payload
func ProcessPullRequestEvent(event github.PullRequestPayload) *WebhookInfo {
	pr := event.PullRequest
	repo := event.Repository
	action := string(event.Action)

	log.Printf("Processing GitHub pull request event: action=%s, PR #%d, repo=%s, branch=%s, sha=%s",
		action, pr.Number, repo.FullName, pr.Head.Ref, pr.Head.Sha)

	prNumber := int(pr.Number)
	return &WebhookInfo{
		EventType:          EventTypePR,
		Owner:              repo.Owner.Login,
		RepositoryName:     repo.Name,
		RepositoryFullName: repo.FullName,
		BranchName:         pr.Head.Ref,
		CommitSHA:          pr.Head.Sha,
		PRNumber:           &prNumber,
		Action:             action,
	}
}

// ProcessPushEvent extracts WebhookInfo from push payload
func ProcessPushEvent(event github.PushPayload) *WebhookInfo {
	repo := event.Repository
	// Extract branch name from ref (e.g., "refs/heads/main" -> "main")
	branchName := strings.TrimPrefix(event.Ref, "refs/heads/")

	log.Printf("Processing GitHub push event: repo=%s, branch=%s, sha=%s",
		repo.FullName, branchName, event.After)

	return &WebhookInfo{
		EventType:          EventTypePush,
		Owner:              repo.Owner.Login,
		RepositoryName:     repo.Name,
		RepositoryFullName: repo.FullName,
		BranchName:         branchName,
		CommitSHA:          event.After,
		PRNumber:           nil, // Not applicable for push events
		Action:             "",  // Not applicable for push events
	}
}
