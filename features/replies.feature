Feature: Replying to review threads
  As an author agent
  I want to reply to review threads
  So that I can respond to feedback without casting a verdict

  Background:
    Given the repo "bobcob7/doc-server" is enrolled with target branch "main"
    And a work branch "wb-9c2f1a" is in state "reviewed"
    And it has a thread opened by the reviewer "ada-lovelace-7-reviewer"
    And I am the author agent "grace-hopper-3-author"

  Scenario: An author replies to a thread immediately
    When I reply to the thread
    Then my reply is visible on the thread right away
    And it was not staged

  Scenario: Replying does not change the work branch state
    When I reply to the thread
    Then the work branch stays in state "reviewed"

  Scenario: Replying does not affect verdicts
    Given the work branch has one "approve" verdict
    When I reply to the thread
    Then the verdicts are unchanged
    And none are marked stale

  Scenario: A reply records the round it was made in
    Given the work branch is on its second review round
    And the thread was raised in the first round
    When I reply to the thread
    Then my reply is recorded against the second round
    And the thread still shows it was raised in the first round

  Scenario: Replying to a missing thread is rejected
    When I reply to a thread that does not exist
    Then the reply is rejected as not found
