Feature: Deciding on proposals
  As an admin
  I want to review reviewed work branches and decide whether they go upstream
  So that only approved changes reach the forge

  Background:
    Given I am signed in to the web interface as the admin
    And the repo "bobcob7/doc-server" is enrolled with target branch "main"

  Scenario: The queue lists reviewed work branches that have an approval
    Given a work branch in state "reviewed" with one "approve" verdict
    And a work branch in state "reviewed" with only a "disapprove" verdict
    When I open the proposal queue
    Then the approved work branch is listed
    And the disapproved work branch is not listed
    And each listed proposal shows its verdicts

  Scenario: Accepting a proposal creates the upstream PR
    Given a proposal in state "reviewed" with one "approve" verdict
    When I accept it
    Then an upstream PR is created with a generated branch name
    And the proposed title and description are the work branch's own
    And the upstream PR URL is recorded on the work branch
    And the work branch stays in state "reviewed"

  Scenario: Accepting requires an approval
    Given a work branch in state "reviewed" with only a "neutral" verdict
    When I try to accept it
    Then the attempt is rejected as a failed precondition

  Scenario: A conflicted work branch cannot be accepted
    Given a work branch flagged as conflicted with its target
    And it is in state "reviewed" with one "approve" verdict
    When I try to accept it
    Then the attempt is rejected as a failed precondition

  Scenario: Requesting a re-review sends the work branch back
    Given a proposal in state "reviewed" with one "approve" verdict
    When I request a re-review
    Then the work branch is in state "reviewable"
    And a new review round is opened
    And the prior verdicts are marked stale
    And it no longer appears in the proposal queue

  Scenario: Closing a work branch ends it
    Given a work branch in state "reviewed"
    When I close it with a reason
    Then the work branch is in state "closed"
    And the reason is recorded on the work branch

  @wip
  Scenario: Closing a work branch closes its upstream PR
    Given a work branch in state "reviewed" whose upstream PR has been created
    When I close it with a reason
    Then the work branch is in state "closed"
    And the upstream PR is closed

  Scenario: A closed upstream PR closes the work branch
    Given a work branch in state "reviewed" whose upstream PR has been created
    When the upstream PR is closed without merging
    Then the next sync sets the work branch to state "closed"

  Scenario: A conflicting target advance removes a proposal from the queue
    Given a proposal in state "reviewed" with one "approve" verdict
    When its target branch advances with conflicting changes
    Then the work branch is in state "draft"
    And it no longer appears in the proposal queue

  Scenario: Re-accepting a caught-up work branch updates the existing PR
    Given a work branch whose upstream PR has been created
    And a conflicting target advance reset it to "draft"
    And it was caught up, re-reviewed, and approved again
    When I accept it
    Then the existing upstream PR is updated in place
    And no new upstream PR is created
