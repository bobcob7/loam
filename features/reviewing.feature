Feature: Reviewing a work branch
  As a reviewer agent
  I want to leave comments and a decisive verdict on a work branch
  So that the author gets actionable feedback and the work can move forward

  Background:
    Given the repo "bobcob7/doc-server" is enrolled with target branch "main"
    And a work branch "wb-9c2f1a" is in state "reviewable"
    And I am the reviewer agent "ada-lovelace-7-reviewer"

  @wip
  Scenario: Finding work awaiting my review
    When I list work branches awaiting my review
    Then "wb-9c2f1a" is included

  @wip
  Scenario: Staged comments are not visible until submitted
    When I stage a comment on a line of the diff
    Then no one else can see the comment
    And I can see it among my staged comments

  @wip
  Scenario: Editing and discarding staged comments before submitting
    Given I have staged two comments
    When I edit one staged comment and discard the other
    Then my staged comments reflect the edit and the removal

  @wip
  Scenario: Submitting a verdict publishes staged comments atomically with an outcome
    Given I have staged two comments
    When I submit a verdict with outcome "disapprove"
    Then both comments become visible on the work branch
    And my staged comments are cleared

  @wip
  Scenario: The first verdict marks the work branch reviewed
    When I submit a verdict with outcome "approve"
    Then the work branch is in state "reviewed"

  @wip
  Scenario: An outcome-only verdict is allowed
    When I submit a verdict with outcome "neutral" and no comments
    Then the verdict is recorded with outcome "neutral"

  @wip
  Scenario: Only the thread's author may resolve it
    Given a thread I opened on the work branch
    And a thread opened by another reviewer
    When I resolve the thread I opened
    Then it is marked resolved
    When I try to resolve the other reviewer's thread
    Then the attempt is rejected

  @wip
  Scenario: Re-submitting replaces my verdict for the round
    Given I submitted a verdict with outcome "disapprove"
    When I submit a verdict with outcome "approve"
    Then my recorded verdict for the round is "approve"

  @wip
  Scenario: Listing verdicts shows each reviewer once, with stale flags
    Given the reviewer "alan-turing-4-reviewer" also submitted an "approve" verdict
    When I list the verdicts
    Then each reviewer appears once with their latest outcome
    And none are marked stale

  @wip
  Scenario: A verdict cannot be submitted before review is requested
    Given a work branch in state "draft"
    When I try to submit a verdict on it
    Then the attempt is rejected as a failed precondition

  @wip
  Scenario: Staged comments survive a new review round
    Given I have staged two comments
    And another reviewer's verdict has marked the work branch "reviewed"
    And the author requests review again
    When I submit a verdict with outcome "neutral"
    Then my comments are published in the new round

  @wip
  Scenario: Verdicts and comments record their review round
    Given the work branch is on its second review round
    When I stage a comment and submit a verdict with outcome "approve"
    Then the verdict is recorded against the second round
    And the published comment is recorded against the second round
