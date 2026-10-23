package sync

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	terrakojoiov1alpha1 "github.com/eeekcct/terrakojo/api/v1alpha1"
	gh "github.com/eeekcct/terrakojo/internal/github"
	ghapi "github.com/google/go-github/v79/github"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var syncLog = logf.Log.WithName("repository-sync")

func FetchDefaultBranchHeadSHA(repo *terrakojoiov1alpha1.Repository, ghClient gh.ClientInterface) (string, error) {
	branch, err := ghClient.GetBranch(repo.Spec.Owner, repo.Spec.Name, repo.Spec.DefaultBranch)
	if err != nil {
		return "", fmt.Errorf("failed to fetch default branch head: %w", err)
	}
	if branch == nil || branch.Commit == nil || branch.Commit.SHA == nil {
		return "", nil
	}
	return *branch.Commit.SHA, nil
}

func isCommitNotFound(err error) bool {
	var ghErr *ghapi.ErrorResponse
	if !errors.As(err, &ghErr) || ghErr.Response == nil {
		return false
	}
	return ghErr.Response.StatusCode == http.StatusNotFound
}

func isBaseCommitMissing(repo *terrakojoiov1alpha1.Repository, ghClient gh.ClientInterface) (bool, error) {
	if repo.Status.LastDefaultBranchHeadSHA == "" {
		return false, nil
	}
	_, err := ghClient.GetCommit(repo.Spec.Owner, repo.Spec.Name, repo.Status.LastDefaultBranchHeadSHA)
	if err == nil {
		return false, nil
	}
	if isCommitNotFound(err) {
		return true, nil
	}
	return false, err
}

// CollectDefaultBranchCommits returns commit SHAs to process.
// When >250 commits exist, returns first 250 and caller should update
// LastDefaultBranchHeadSHA to the last returned SHA for incremental processing.
func CollectDefaultBranchCommits(repo *terrakojoiov1alpha1.Repository, ghClient gh.ClientInterface, headSHA string) ([]string, error) {
	if headSHA == "" {
		return nil, nil
	}
	if repo.Status.LastDefaultBranchHeadSHA == "" {
		return []string{headSHA}, nil
	}
	if repo.Status.LastDefaultBranchHeadSHA == headSHA {
		return nil, nil
	}

	// Use CompareCommits to detect truncation
	result, err := ghClient.CompareCommits(repo.Spec.Owner, repo.Spec.Name, repo.Status.LastDefaultBranchHeadSHA, headSHA)
	if err != nil {
		// CompareCommits can fail for transient reasons (rate limit, 5xx, network).
		// Only fall back to headSHA when the base commit no longer exists, which
		// indicates a history rewrite (e.g., force-push). Otherwise return the
		// error to retry and avoid skipping intermediate commits.
		missing, lookupErr := isBaseCommitMissing(repo, ghClient)
		if lookupErr != nil {
			return nil, lookupErr
		}
		if missing {
			return []string{headSHA}, nil
		}
		return nil, err
	}

	shas := make([]string, 0, len(result.Commits))
	for _, commit := range result.Commits {
		if commit == nil || commit.SHA == nil {
			continue
		}
		shas = append(shas, *commit.SHA)
	}
	if len(shas) == 0 {
		if result.Status == "behind" {
			return []string{headSHA}, nil
		}
		// CompareCommits returned successfully but without commits. Only fall back
		// when the base commit no longer exists; otherwise retry to avoid skipping
		// intermediate commits when histories diverge.
		missing, lookupErr := isBaseCommitMissing(repo, ghClient)
		if lookupErr != nil {
			return nil, lookupErr
		}
		if missing {
			return []string{headSHA}, nil
		}
		return nil, fmt.Errorf("github CompareCommits returned no commits between %s and %s", repo.Status.LastDefaultBranchHeadSHA, headSHA)
	}
	// When truncated, remaining commits will be processed in next reconcile
	// after LastDefaultBranchHeadSHA is updated to the last SHA in this batch
	return shas, nil
}

func FetchBranchHeadsFromGitHub(repo *terrakojoiov1alpha1.Repository, ghClient gh.ClientInterface) (*BranchHeads, error) {
	branches, err := ghClient.ListBranches(repo.Spec.Owner, repo.Spec.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to list branches: %w", err)
	}

	heads := map[string]BranchHead{}
	localSHAs := map[string]string{}
	for _, branch := range branches {
		if branch == nil || branch.GetName() == "" || branch.GetCommit().GetSHA() == "" {
			continue
		}
		ref := branch.GetName()
		if ref == repo.Spec.DefaultBranch {
			continue
		}
		localSHAs[ref] = branch.GetCommit().GetSHA()
		heads[ref] = BranchHead{BranchInfo: terrakojoiov1alpha1.BranchInfo{
			Ref: ref, SHA: branch.GetCommit().GetSHA(),
		}}
	}

	prs, err := ghClient.ListOpenPullRequests(repo.Spec.Owner, repo.Spec.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to list pull requests: %w", err)
	}

	snapshot := &BranchHeads{ExternalPRs: make(map[int]string)}
	for _, pr := range prs {
		if pr == nil || pr.Head == nil || pr.Head.GetRef() == "" || pr.GetNumber() <= 0 {
			continue
		}
		head := BranchHead{
			BranchInfo: terrakojoiov1alpha1.BranchInfo{
				Ref: pr.Head.GetRef(), SHA: pr.Head.GetSHA(), PRNumber: pr.GetNumber(),
			},
			ExternalPR: !isSameRepositoryPR(repo, pr),
		}
		if head.ExternalPR {
			snapshot.ExternalPRs[head.PRNumber] = head.Ref
		}
		key := head.Key()
		if existing, exists := heads[key]; exists {
			// Preserve highest-PR selection only for the same local ref.
			if head.PRNumber <= existing.PRNumber {
				continue
			}
		}
		if head.SHA == "" && !head.ExternalPR {
			head.SHA = localSHAs[head.Ref]
		}
		heads[key] = head
	}

	keys := make([]string, 0, len(heads))
	for key := range heads {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	snapshot.Heads = make([]BranchHead, 0, len(keys))
	for _, key := range keys {
		head := heads[key]
		if head.SHA == "" {
			syncLog.Info("Skipping branch ref with missing SHA from GitHub", "ref", head.Ref, "prNumber", head.PRNumber)
			continue
		}
		snapshot.Heads = append(snapshot.Heads, head)
	}
	return snapshot, nil
}

// GitHub's PR-list endpoint is scoped to the configured base repository.
// Repository IDs, when present on both sides, survive renames. Older or partial
// responses may omit IDs; require explicit head repository names in that case.
// A deleted/unknown head repository must never inherit a local branch identity.
func isSameRepositoryPR(repo *terrakojoiov1alpha1.Repository, pr *ghapi.PullRequest) bool {
	headRepo := pr.GetHead().GetRepo()
	if headRepo == nil {
		return false
	}
	baseRepo := pr.GetBase().GetRepo()
	if headRepo.GetID() > 0 && baseRepo.GetID() > 0 {
		return headRepo.GetID() == baseRepo.GetID()
	}
	expected := repo.Spec.Owner + "/" + repo.Spec.Name
	if headRepo.GetFullName() != "" {
		return strings.EqualFold(headRepo.GetFullName(), expected)
	}
	return strings.EqualFold(headRepo.GetOwner().GetLogin(), repo.Spec.Owner) &&
		strings.EqualFold(headRepo.GetName(), repo.Spec.Name)
}
