package fakeforge

import "sync"

// prRecord is the fake forge's in-memory record of one pull request.
type prRecord struct {
	number       int
	headBranch   string
	targetBranch string
	title        string
	description  string
	state        string // "open" | "merged" | "closed"
}

// prRegistry tracks pull requests per repo. It is its own small piece of
// state, separate from token storage, so control/provider operations on
// PRs never contend with git auth lookups.
type prRegistry struct {
	mu   sync.Mutex
	next map[string]int
	byNo map[string]map[int]*prRecord
}

func newPRRegistry() *prRegistry {
	return &prRegistry{next: make(map[string]int), byNo: make(map[string]map[int]*prRecord)}
}

// create allocates the next PR number for repo and records it as open.
func (r *prRegistry) create(repo, headBranch, targetBranch, title, description string) *prRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next[repo]++
	pr := &prRecord{number: r.next[repo], headBranch: headBranch, targetBranch: targetBranch, title: title, description: description, state: "open"}
	if r.byNo[repo] == nil {
		r.byNo[repo] = make(map[int]*prRecord)
	}
	r.byNo[repo][pr.number] = pr
	return pr
}

// get returns the PR record for repo/number, or ok=false if unknown.
func (r *prRegistry) get(repo string, number int) (*prRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pr, ok := r.byNo[repo][number]
	return pr, ok
}

// findOpen returns the open PR (if any) already recorded for repo with the
// given head/target branch pair. This is what lets handleCreatePR detect a
// repeat CreatePR the way real Forgejo does: a second request for a pair
// that already has an open PR is a conflict, not a fresh PR number. A PR
// that has since closed or merged does not block a new one for the same
// pair, matching Forgejo allowing re-opening after a prior PR concluded.
func (r *prRegistry) findOpen(repo, headBranch, targetBranch string) (*prRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, pr := range r.byNo[repo] {
		if pr.state == "open" && pr.headBranch == headBranch && pr.targetBranch == targetBranch {
			return pr, true
		}
	}
	return nil, false
}

// list snapshots every PR recorded for repo as a caller-owned slice,
// ordered by number ascending. Numbers are allocated densely from 1 by
// create, so iterating 1..next is a total ordering with no gaps and no
// sort; a number the map does not hold would mean an allocation that never
// recorded, which cannot happen under the same mutex.
func (r *prRegistry) list(repo string) []PullRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	byNo := r.byNo[repo]
	if len(byNo) == 0 {
		return nil
	}
	out := make([]PullRequest, 0, len(byNo))
	for number := 1; number <= r.next[repo]; number++ {
		pr, ok := byNo[number]
		if !ok {
			continue
		}
		out = append(out, PullRequest{
			Number:       pr.number,
			HeadBranch:   pr.headBranch,
			TargetBranch: pr.targetBranch,
			Title:        pr.title,
			Description:  pr.description,
			State:        pr.state,
		})
	}
	return out
}

// forget drops every PR recorded for repo, including its number
// allocator, so a later repo of the same name starts from #1 with an
// empty registry. It exists for Server.RemoveRepo: leaving the records
// behind would let a GetPRState for a long-gone repo answer with a
// stale terminal state instead of errPRNotFound.
func (r *prRegistry) forget(repo string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.next, repo)
	delete(r.byNo, repo)
}

// setState transitions the PR's state; a no-op if the PR is unknown.
func (r *prRegistry) setState(repo string, number int, state string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if pr, ok := r.byNo[repo][number]; ok {
		pr.state = state
	}
}
