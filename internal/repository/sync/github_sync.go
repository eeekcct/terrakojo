package sync

import (
	"errors"
	"fmt"
	"net/http"
	"sort"

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

func FetchBranchHeadsFromGitHub(repo *terrakojoiov1alpha1.Repository, ghClient gh.ClientInterface) ([]terrakojoiov1alpha1.BranchInfo, error) {
	branches, err := ghClient.ListBranches(repo.Spec.Owner, repo.Spec.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to list branches: %w", err)
	}

	branchHeads := map[string]string{}
	for _, branch := range branches {
		if branch == nil || branch.Name == nil || branch.Commit == nil || branch.Commit.SHA == nil {
			continue
		}
		ref := *branch.Name
		if ref == repo.Spec.DefaultBranch {
			continue
		}
		branchHeads[ref] = *branch.Commit.SHA
	}

	prs, err := ghClient.ListOpenPullRequests(repo.Spec.Owner, repo.Spec.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to list pull requests: %w", err)
	}

	type prHead struct {
		number int
		sha    string
	}
	prByRef := map[string]prHead{}
	for _, pr := range prs {
		if pr == nil || pr.Head == nil || pr.Head.Ref == nil || pr.Number == nil {
			continue
		}
		ref := *pr.Head.Ref
		if existing, exists := prByRef[ref]; exists {
			// Multiple PRs exist for the same ref; keep the one with the higher number
			if *pr.Number <= existing.number {
				continue
			}
			// Silently prefer higher PR number when multiple PRs exist for same ref
		}
		sha := ""
		if pr.Head.SHA != nil {
			sha = *pr.Head.SHA
		}
		prByRef[ref] = prHead{number: *pr.Number, sha: sha}
	}

	refSet := make(map[string]struct{}, len(branchHeads)+len(prByRef))
	for ref := range branchHeads {
		refSet[ref] = struct{}{}
	}
	for ref := range prByRef {
		refSet[ref] = struct{}{}
	}

	newRefs := make([]string, 0, len(refSet))
	for ref := range refSet {
		newRefs = append(newRefs, ref)
	}
	sort.Strings(newRefs)
	updated := make([]terrakojoiov1alpha1.BranchInfo, 0, len(newRefs))
	for _, ref := range newRefs {
		sha := branchHeads[ref]
		prInfo, hasPR := prByRef[ref]
		// Prefer PR head SHA when a PR exists for this ref.
		// This ensures workflows run against the actual PR code,
		// not a potentially stale branch SHA or a base repo branch.
		// If PR SHA is unavailable, fall back to branch SHA.
		if hasPR && prInfo.sha != "" {
			sha = prInfo.sha
		}
		if sha == "" {
			if hasPR {
				syncLog.Info("Skipping branch ref with missing SHA from GitHub", "ref", ref, "prNumber", prInfo.number)
			} else {
				syncLog.Info("Skipping branch ref with missing SHA from GitHub", "ref", ref)
			}
			continue
		}
		entry := terrakojoiov1alpha1.BranchInfo{
			Ref: ref,
			SHA: sha,
		}
		if hasPR {
			entry.PRNumber = prInfo.number
		}
		updated = append(updated, entry)
	}
	return updated, nil
}
