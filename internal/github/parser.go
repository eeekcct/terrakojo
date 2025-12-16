package github

import (
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
	BaseBranchName     string           `json:"baseBranchName,omitempty"`
	BranchName         string           `json:"branchName,omitempty"`
	CommitSHA          string           `json:"commitSha,omitempty"`
	MergeCommitSHA     string           `json:"mergeCommitSha,omitempty"`
	PRNumber           *int             `json:"prNumber,omitempty"`
	Action             string           `json:"action,omitempty"` // for PR events: opened, closed, etc.
	Merged             bool             `json:"merged,omitempty"`
}

// ProcessPullRequestEvent extracts WebhookInfo from pull request payload
func ProcessPullRequestEvent(event github.PullRequestPayload) *WebhookInfo {
	pr := event.PullRequest
	repo := event.Repository
	action := string(event.Action)

	mergeSHA := ""
	if pr.MergeCommitSha != nil {
		mergeSHA = *pr.MergeCommitSha
	}

	prNumber := int(pr.Number)
	return &WebhookInfo{
		EventType:          EventTypePR,
		Owner:              repo.Owner.Login,
		RepositoryName:     repo.Name,
		RepositoryFullName: repo.FullName,
		BaseBranchName:     pr.Base.Ref,
		BranchName:         pr.Head.Ref,
		CommitSHA:          pr.Head.Sha,
		MergeCommitSHA:     mergeSHA,
		PRNumber:           &prNumber,
		Action:             action,
		Merged:             pr.Merged,
	}
}

// ProcessPushEvent extracts WebhookInfo from push payload
func ProcessPushEvent(event github.PushPayload) *WebhookInfo {
	repo := event.Repository
	// Extract branch name from ref (e.g., "refs/heads/main" -> "main")
	branchName := strings.TrimPrefix(event.Ref, "refs/heads/")

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
