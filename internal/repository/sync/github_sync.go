package sync

import (
	"fmt"
	"sort"

	terrakojoiov1alpha1 "github.com/eeekcct/terrakojo/api/v1alpha1"
	gh "github.com/eeekcct/terrakojo/internal/github"
)

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
		// Fallback to headSHA if compare fails (e.g., force-push rewrote history).
		// This ensures default-branch sync continues even after non-fast-forward updates.
		return []string{headSHA}, nil
	}

	shas := make([]string, 0, len(result.Commits))
	for _, commit := range result.Commits {
		if commit == nil || commit.SHA == nil {
			continue
		}
		shas = append(shas, *commit.SHA)
	}
	if len(shas) == 0 {
		return []string{headSHA}, nil
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
		if hasPR {
			sha = prInfo.sha
		}
		if sha == "" {
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
