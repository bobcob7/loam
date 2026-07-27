package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// The on-disk staging format. This file OWNS it: `work comment` writes it,
// `work comments --staged` reads it, and `work verdict` publishes and then
// clears it (docs/cli-spec.md -> comment (add), comments (get), verdict).
//
// One JSON document per (repo, work-branch, agent) staging directory rather
// than one file per staged item, for two reasons. First, StagingArea
// deliberately exposes no directory listing — it is a contained
// read/write/remove handle, nothing more — so a per-item layout could not be
// enumerated without widening that seam. Second, a single document makes id
// allocation, edit, and discard one load-modify-write against a consistent
// snapshot instead of a directory scan whose result depends on what other
// entries happen to be present.
const (
	stagedFileName      = "staged.json"
	stagedFormatVersion = 1
	stagedIDPrefix      = "s"
)

// stagedItem is one staged entry: a new thread the caller is about to open
// (Body, optionally anchored at File/Line), a thread the caller is about to
// mark resolved (Resolve), or both at once — docs/cli-spec.md lets
// `--resolve` accompany a new comment.
//
// There is deliberately NO round field. A staged item is inert local data
// that survives round changes (docs/cli-spec.md -> State gates); the round
// is assigned only when `work verdict` publishes it, which is why "Staged
// comments survive a new review round" needs no bookkeeping here.
//
// There is deliberately no author field either: the staging directory is
// already keyed by the agent identifier, so every item in a given file has
// exactly one possible author and recording it per item could only ever
// disagree with the directory it lives in.
type stagedItem struct {
	ID      string `json:"id"`
	File    string `json:"file,omitempty"`
	Line    uint32 `json:"line,omitempty"`
	Body    string `json:"body,omitempty"`
	Resolve string `json:"resolve,omitempty"`
}

// stagedSet is the whole staged.json document. NextID is persisted rather
// than derived from Items so an id is never reused after the item holding it
// is discarded: reuse would silently re-point a later `--edit s3` at a
// different comment than the agent read out of an earlier invocation.
type stagedSet struct {
	Version int          `json:"version"`
	NextID  int          `json:"next_id"`
	Items   []stagedItem `json:"items"`
}

// stagingStore is the load-modify-write layer over a StagingArea. Every
// operation re-reads staged.json and writes it back, so staged items
// accumulate across CLI invocations (docs/cli-spec.md -> comment (add):
// "Staged items accumulate across invocations") with no process-lifetime
// state at all.
type stagingStore struct{ area StagingArea }

// openStagingStore opens the caller's staging area for repo/workBranch and
// wraps it in a store. It goes through WorkspaceResolver.OpenStaging — the
// only way to reach staged files — so every read and write below is
// confined to that directory at the syscall level. Callers must Close it.
func openStagingStore(ws WorkspaceResolver, repo, workBranch string) (*stagingStore, error) {
	area, err := ws.OpenStaging(repo, workBranch)
	if err != nil {
		return nil, err
	}
	return &stagingStore{area: area}, nil
}

// Close releases the underlying staging area handle.
func (s *stagingStore) Close() error { return s.area.Close() }

// load reads staged.json. A staging area with no staged.json yet is an
// empty set, not an error: the first `work comment` in a work branch always
// finds one.
func (s *stagingStore) load() (*stagedSet, error) {
	data, err := s.area.ReadFile(stagedFileName)
	if errors.Is(err, os.ErrNotExist) {
		return &stagedSet{Version: stagedFormatVersion, NextID: 1}, nil
	}
	if err != nil {
		return nil, err
	}
	var set stagedSet
	if err := json.Unmarshal(data, &set); err != nil {
		return nil, fmt.Errorf("%w: parsing %s: %w", errStagingArea, stagedFileName, err)
	}
	if set.Version != stagedFormatVersion {
		return nil, fmt.Errorf("%w: %s has format version %d, this loam understands %d", errStagingArea, stagedFileName, set.Version, stagedFormatVersion)
	}
	normalizeNextID(&set)
	return &set, nil
}

// normalizeNextID raises NextID above every id already present. The file is
// plain JSON in the agent's own workspace, so a hand-edited or truncated
// next_id is reachable; without this, the next allocation would hand out an
// id that is already taken and `--edit`/`--discard` would then address two
// different items by one name.
func normalizeNextID(set *stagedSet) {
	for _, item := range set.Items {
		n, err := strconv.Atoi(strings.TrimPrefix(item.ID, stagedIDPrefix))
		if err != nil {
			continue
		}
		if n >= set.NextID {
			set.NextID = n + 1
		}
	}
	if set.NextID < 1 {
		set.NextID = 1
	}
}

// save writes set back to staged.json, stamping the current format version
// so a document this build produced always identifies itself.
func (s *stagingStore) save(set *stagedSet) error {
	set.Version = stagedFormatVersion
	data, err := json.MarshalIndent(set, "", "  ")
	if err != nil {
		return fmt.Errorf("%w: encoding %s: %w", errStagingArea, stagedFileName, err)
	}
	return s.area.WriteFile(stagedFileName, append(data, '\n'))
}

// list returns the caller's staged items in staging order — what `work
// comments --staged` reports and what `work verdict` publishes.
func (s *stagingStore) list() ([]stagedItem, error) {
	set, err := s.load()
	if err != nil {
		return nil, err
	}
	return set.Items, nil
}

// add appends item under a freshly allocated local id ("s1", "s2", …) and
// returns it as stored.
func (s *stagingStore) add(item stagedItem) (stagedItem, error) {
	set, err := s.load()
	if err != nil {
		return stagedItem{}, err
	}
	item.ID = stagedIDPrefix + strconv.Itoa(set.NextID)
	set.NextID++
	set.Items = append(set.Items, item)
	if err := s.save(set); err != nil {
		return stagedItem{}, err
	}
	return item, nil
}

// edit replaces the body of the staged item id, leaving its anchor, its
// resolve target, and its position in the set untouched. An unknown id is
// not_found (exit 3, docs/cli-spec.md -> comment (add) -> Errors).
func (s *stagingStore) edit(id, body string) (stagedItem, error) {
	set, err := s.load()
	if err != nil {
		return stagedItem{}, err
	}
	idx, err := indexOfStagedItem(set, id)
	if err != nil {
		return stagedItem{}, err
	}
	set.Items[idx].Body = body
	if err := s.save(set); err != nil {
		return stagedItem{}, err
	}
	return set.Items[idx], nil
}

// discard removes the staged item id and returns the item as it was before
// removal, so the caller can report exactly what it dropped. An unknown id
// is not_found (exit 3).
func (s *stagingStore) discard(id string) (stagedItem, error) {
	set, err := s.load()
	if err != nil {
		return stagedItem{}, err
	}
	idx, err := indexOfStagedItem(set, id)
	if err != nil {
		return stagedItem{}, err
	}
	removed := set.Items[idx]
	set.Items = append(set.Items[:idx], set.Items[idx+1:]...)
	if err := s.save(set); err != nil {
		return stagedItem{}, err
	}
	return removed, nil
}

// indexOfStagedItem locates id in set, or reports not_found (exit 3) naming
// the id the caller asked for.
func indexOfStagedItem(set *stagedSet, id string) (int, error) {
	for i, item := range set.Items {
		if item.ID == id {
			return i, nil
		}
	}
	return 0, newNotFoundError(fmt.Sprintf("no staged comment %q in this work branch's staging area", id), nil)
}
