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

// setState transitions the PR's state; a no-op if the PR is unknown.
func (r *prRegistry) setState(repo string, number int, state string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if pr, ok := r.byNo[repo][number]; ok {
		pr.state = state
	}
}
