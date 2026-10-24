package sync

import (
	"strconv"

	terrakojoiov1alpha1 "github.com/eeekcct/terrakojo/api/v1alpha1"
)

// BranchHead separates reconciliation identity from the literal Git ref.
// External (including unknown-origin) PRs are scoped to their PR number in the
// base repository. Local branches retain their existing per-ref identity.
type BranchHead struct {
	terrakojoiov1alpha1.BranchInfo
	ExternalPR bool
}

func (head BranchHead) Key() string {
	if head.ExternalPR {
		// A colon cannot occur in a Git branch ref.
		return "pr:" + strconv.Itoa(head.PRNumber)
	}
	return head.Ref
}

// BranchHeads is one GitHub snapshot. ExternalPRs also includes PRs whose SHA
// is unavailable, so a legacy fork Branch cannot be mistaken for a local ref.
type BranchHeads struct {
	Heads       []BranchHead
	ExternalPRs map[int]string
}
