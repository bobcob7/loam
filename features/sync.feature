Feature: Upstream sync
  As an admin
  I want the server to mirror upstream faithfully and track my proposals' PRs
  So that Loam always reflects the real state of the forge

  Background:
    Given I am signed in to the web interface as the admin
    And the repo "bobcob7/doc-server" is enrolled with target branch "main"

  @wip
  Scenario: The mirror follows upstream, always
    Given the mirror's "main" has diverged from upstream
    When the next sync runs
    Then the mirror's "main" matches upstream exactly

  @wip
  Scenario: A branch deleted upstream is pruned from the mirror
    Given upstream has deleted its branch "feature-x"
    When the next sync runs
    Then the mirror no longer has "feature-x"

  @wip
  Scenario: Sync never touches work branch refs
    Given a work branch "wb-9c2f1a" exists
    And upstream has a branch named "wb-9c2f1a"
    When the next sync runs
    Then the work branch "wb-9c2f1a" is unchanged in the mirror

  @wip
  Scenario: A missing target branch is surfaced as a sync error
    Given upstream has deleted the target branch "main"
    When the next sync runs
    Then the repo's sync status shows an error naming "main"
    And existing work branches are left untouched

  @wip
  Scenario: Sync failures are retried on the next cycle
    Given the upstream forge is unreachable
    When the next sync runs
    Then the repo's sync status shows the error
    When the forge is reachable again and the next sync runs
    Then the repo's sync status is healthy

  @wip
  Scenario: Accepting pushes a namespaced branch upstream
    Given a proposal in state "reviewed" with one "approve" verdict
    When I accept it
    Then a branch prefixed "loam/" is pushed to the upstream forge
    And the upstream PR is opened from that branch into "main"

  @wip
  Scenario: The PR body attributes Loam and only Loam
    Given a proposal in state "reviewed" with one "approve" verdict
    When I accept it
    Then the PR body is the work branch's description
    And it ends with a footer attributing the PR to Loam
    And no agent identity appears in the body

  @wip
  Scenario: PR attribution can be disabled
    Given the server is configured without PR attribution
    And a proposal in state "reviewed" with one "approve" verdict
    When I accept it
    Then the PR body is the work branch's description alone

  @wip
  Scenario: The pushed branch is cleaned up after the PR ends
    Given an accepted work branch whose upstream PR has merged
    When the next sync runs
    Then the work branch is in state "complete"
    And the "loam/" branch is removed from the upstream forge
