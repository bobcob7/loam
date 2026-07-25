Feature: Work branch lifecycle
  As an author agent
  I want to open a work branch for review and iterate on feedback
  So that my changes can be approved and proposed upstream

  Background:
    Given the repo "bobcob7/doc-server" is enrolled with target branch "main"
    And I am the author agent "grace-hopper-3-author"

  @wip
  Scenario: Starting a work branch
    When I start a work branch from "main"
    Then a work branch is created in state "draft"
    And its name is randomly generated

  @wip
  Scenario: A work branch cannot be reviewed without a title and description
    Given I have started a work branch with no title or description
    When I request review
    Then the request is rejected with a precondition error
    And the work branch stays in state "draft"

  @wip
  Scenario: Requesting review opens the work branch for review
    Given I have started a work branch with a title and description
    When I request review
    Then the work branch is in state "reviewable"

  @wip
  Scenario: The title and description can change while work progresses
    Given a work branch in state "reviewable"
    When I update its title and description
    Then the work branch keeps its state "reviewable"
    And the new title and description are shown

  @wip
  Scenario: The first verdict marks the work branch reviewed
    Given a work branch in state "reviewable"
    When the reviewer "ada-lovelace-7-reviewer" submits an "approve" verdict
    Then the work branch is in state "reviewed"

  @wip
  Scenario: A reviewed work branch with an approval becomes a proposal
    Given a work branch in state "reviewed" with one "approve" verdict
    Then it appears in the admin's proposal queue

  @wip
  Scenario: Requesting review again starts a fresh round and marks prior verdicts stale
    Given a work branch in state "reviewed" with one "approve" verdict
    When I request review again
    Then the work branch is in state "reviewable"
    And the prior verdicts are marked stale
    And it no longer appears in the admin's proposal queue

  @wip
  Scenario: Completion happens only when the upstream PR merges
    Given a work branch in state "reviewed" whose upstream PR has been created
    When the upstream PR merges
    Then the work branch is in state "complete"

  @wip
  Scenario: An author cannot mark a work branch complete
    Given a work branch in state "reviewed"
    Then there is no author action that sets it "complete"

  @wip
  Scenario: A terminal work branch cannot be edited
    Given a work branch in state "closed"
    When I try to update its title
    Then the attempt is rejected as a failed precondition

  @wip
  Scenario: A clean target advance leaves the work branch untouched
    Given a work branch in state "reviewable"
    When the target branch advances with changes that merge cleanly
    Then the work branch's commits are unchanged
    And it keeps its state "reviewable"

  @wip
  Scenario: A conflicting target advance resets the work branch to draft
    Given a work branch in state "reviewed" with one "approve" verdict
    When the target branch advances with conflicting changes
    Then the work branch is in state "draft"
    And it is flagged as conflicted
    And the prior verdicts are marked stale

  @wip
  Scenario: Catching up returns a conflict-reset work branch to review
    Given a work branch reset to "draft" by a conflicting target advance
    When I push commits that bring it up to date with its target
    Then the work branch is in state "reviewable"
    And no request for review was needed
