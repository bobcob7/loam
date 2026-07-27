Feature: Cloning and pushing work
  As an author agent
  I want to clone a work branch and push my commits with plain git
  So that my work reaches the server for review

  Background:
    Given the repo "bobcob7/doc-server" is enrolled with target branch "main"
    And I am the author agent "grace-hopper-3-author"
    And I have started the work branch "wb-9c2f1a"

  Scenario: Cloning a work branch bootstraps plain git
    When I clone "bobcob7/doc-server" at "wb-9c2f1a"
    Then the clone is placed at "./doc-server"
    And its only remote is the Loam server
    And its git author is set to my agent identity
    And my identity is carried on every git operation from the clone

  Scenario: Pushing commits with plain git
    Given I am in the clone checked out on "wb-9c2f1a"
    When I commit and push
    Then my commits reach the server on "wb-9c2f1a"

  Scenario: Target branches are read-only
    When I push to the target branch "main"
    Then the push is rejected as read-only

  @wip
  Scenario: Pushes cannot create branches
    When I push a branch that is not a registered work branch
    Then the push is rejected

  @wip
  Scenario: Only the author may push to a work branch
    Given the work branch "wb-4d21aa" belongs to another agent
    When I push to "wb-4d21aa"
    Then the push is rejected

  @wip
  Scenario: A terminal work branch rejects pushes
    Given the work branch "wb-9c2f1a" is in state "closed"
    When I push to "wb-9c2f1a"
    Then the push is rejected

  @wip
  Scenario: Force pushes are rejected
    Given I have rewritten the history of "wb-9c2f1a" locally
    When I force push
    Then the push is rejected

  Scenario: A push without a Loam identity is rejected
    Given a clone whose git configuration carries no agent identity
    When I push from it
    Then the push is rejected
