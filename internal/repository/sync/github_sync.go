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

	commits, err := ghClient.CompareCommits(repo.Spec.Owner, repo.Spec.Name, repo.Status.LastDefaultBranchHeadSHA, headSHA)
	if err != nil {
		return []string{headSHA}, nil
	}

	shas := make([]string, 0, len(commits))
	for _, commit := range commits {
		if commit == nil || commit.SHA == nil {
			continue
		}
		shas = append(shas, *commit.SHA)
	}
	if len(shas) == 0 {
		return []string{headSHA}, nil
	}
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

	prByRef := map[string]int{}
	for _, pr := range prs {
		if pr == nil || pr.Head == nil || pr.Head.Ref == nil || pr.Number == nil {
			continue
		}
		ref := *pr.Head.Ref
		if _, exists := prByRef[ref]; exists {
			continue
		}
		prByRef[ref] = *pr.Number
	}

	newRefs := make([]string, 0, len(branchHeads))
	for ref := range branchHeads {
		newRefs = append(newRefs, ref)
	}
	sort.Strings(newRefs)
	updated := make([]terrakojoiov1alpha1.BranchInfo, 0, len(newRefs))
	for _, ref := range newRefs {
		entry := terrakojoiov1alpha1.BranchInfo{
			Ref: ref,
			SHA: branchHeads[ref],
		}
		if prNumber, ok := prByRef[ref]; ok {
			entry.PRNumber = prNumber
		}
		updated = append(updated, entry)
	}
	return updated, nil
}
