Feature: Upstream sync
  As an admin
  I want the server to mirror upstream faithfully and track my proposals' PRs
  So that Loam always reflects the real state of the forge

  Background:
    Given I am signed in to the web interface as the admin
    And the repo "bobcob7/doc-server" is enrolled with target branch "main"

  Scenario: The mirror follows upstream, always
    Given the mirror's "main" has diverged from upstream
    When the next sync runs
    Then the mirror's "main" matches upstream exactly

  Scenario: A branch deleted upstream is pruned from the mirror
    Given upstream has deleted its branch "feature-x"
    When the next sync runs
    Then the mirror no longer has "feature-x"

  Scenario: Sync never touches work branch refs
    Given a work branch "wb-9c2f1a" exists
    And upstream has a branch named "wb-9c2f1a"
    When the next sync runs
    Then the work branch "wb-9c2f1a" is unchanged in the mirror

  Scenario: A missing target branch is surfaced as a sync error
    Given upstream has deleted the target branch "main"
    When the next sync runs
    Then the repo's sync status shows an error naming "main"
    And existing work branches are left untouched

  Scenario: Sync failures are retried on the next cycle
    Given the upstream forge is unreachable
    When the next sync runs
    Then the repo's sync status shows the error
    When the forge is reachable again and the next sync runs
    Then the repo's sync status is healthy

  Scenario: Accepting pushes a namespaced branch upstream
    Given a proposal in state "reviewed" with one "approve" verdict
    When I accept it
    Then a branch prefixed "loam/" is pushed to the upstream forge
    And the upstream PR is opened from that branch into "main"

  Scenario: The PR body attributes Loam and only Loam
    Given a proposal in state "reviewed" with one "approve" verdict
    When I accept it
    Then the PR body is the work branch's description
    And it ends with a footer attributing the PR to Loam
    And no agent identity appears in the body

  Scenario: PR attribution can be disabled
    Given the server is configured without PR attribution
    And a proposal in state "reviewed" with one "approve" verdict
    When I accept it
    Then the PR body is the work branch's description alone

  Scenario: The pushed branch is cleaned up after the PR ends
    Given an accepted work branch whose upstream PR has merged
    When the next sync runs
    Then the work branch is in state "complete"
    And the "loam/" branch is removed from the upstream forge

  Scenario: A commit pushed straight to the upstream branch is adopted, and the approvals reset
    Given a proposal in state "reviewed" with one "approve" verdict
    And I accept it
    And someone pushes a commit directly to the upstream "loam/" branch
    When the next sync runs
    Then the work branch advances to that commit
    And a new review round is opened by the server
    And the accepted tip records that commit
    And accepting it is refused until it is approved again

  Scenario: An upstream branch someone rewrote is flagged for the admin, never guessed at
    Given a proposal in state "reviewed" with one "approve" verdict
    And I accept it
    And the upstream "loam/" branch moves on while the work branch moves on separately
    When the next sync runs
    Then the work branch still holds the commit its author pushed
    And the admin sees the work branch flagged as diverged from upstream
    When I try to accept it
    Then the attempt is rejected as a failed precondition

  Scenario: An upstream branch rewound behind the work branch is flagged, not re-adopted
    Given a proposal in state "reviewed" with one "approve" verdict
    And I accept it
    And the upstream "loam/" branch is rewound behind the work branch
    When the next sync runs
    Then the work branch still holds the commit its author pushed
    And the admin sees the work branch flagged as diverged from upstream
    When I try to accept it
    Then the attempt is rejected as a failed precondition
