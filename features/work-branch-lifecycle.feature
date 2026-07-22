Feature: Work branch lifecycle
  As an author agent
  I want to open a work branch for review and iterate on feedback
  So that my changes can be approved and proposed upstream

  Background:
    Given the repo "bobcob7/doc-server" is enrolled with target branch "main"
    And I am the author agent "grace-hopper-3-author"

  Scenario: Starting a work branch
    When I start a work branch from "main"
    Then a work branch is created in state "draft"
    And its name is randomly generated

  Scenario: A work branch cannot be reviewed without a title and description
    Given I have started a work branch with no title or description
    When I request review
    Then the request is rejected with a precondition error
    And the work branch stays in state "draft"

  Scenario: Requesting review opens the work branch for review
    Given I have started a work branch with a title and description
    When I request review
    Then the work branch is in state "reviewable"

  Scenario: The title and description can change while work progresses
    Given a work branch in state "reviewable"
    When I update its title and description
    Then the work branch keeps its state "reviewable"
    And the new title and description are shown

  Scenario: The first verdict marks the work branch reviewed
    Given a work branch in state "reviewable"
    When the reviewer "ada-lovelace-7-reviewer" submits an "approve" verdict
    Then the work branch is in state "reviewed"

  Scenario: A reviewed work branch with an approval becomes a proposal
    Given a work branch in state "reviewed" with one "approve" verdict
    Then it appears in the admin's proposal queue

  Scenario: Requesting review again starts a fresh round and marks prior verdicts stale
    Given a work branch in state "reviewed" with one "approve" verdict
    When I request review again
    Then the work branch is in state "reviewable"
    And the prior verdicts are marked stale
    And it no longer appears in the admin's proposal queue

  Scenario: Completion happens only when the upstream PR merges
    Given a work branch in state "reviewed" whose upstream PR has been created
    When the upstream PR merges
    Then the work branch is in state "complete"

  Scenario: An author cannot mark a work branch complete
    Given a work branch in state "reviewed"
    Then there is no author action that sets it "complete"
