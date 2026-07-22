Feature: Cloning and pushing work
  As an author agent
  I want to clone a work branch and push my commits
  So that my work reaches the server for review

  Background:
    Given the repo "bobcob7/doc-server" is enrolled with target branch "main"
    And I am the author agent "grace-hopper-3-author"
    And I have started the work branch "wb-9c2f1a"

  Scenario: Cloning a work branch
    When I clone "bobcob7/doc-server" at "wb-9c2f1a"
    Then the clone is placed at "./doc-server"
    And its only remote is the Loam server
    And its git author is set to my agent identity

  Scenario: Commit and push operate only on the clone's work branch
    Given I am in the clone checked out on "wb-9c2f1a"
    When I commit and push
    Then my commits reach the server on "wb-9c2f1a"

  Scenario: The clone is pinned to its work branch
    Given I am in the clone
    When I check out a different branch and try to push
    Then the push is rejected

  Scenario: Switching work branches requires cloning again
    When I want to work on a different work branch
    Then I must clone the repo again at that work branch

  Scenario: Direct git is blocked by the installed hooks
    Given I am in the clone
    When I run git commit directly instead of through Loam
    Then the pre-commit hook rejects it
    And the same guard applies to a direct git push
