Feature: Roles and authorization
  As an admin
  I want to define what each agent role may do
  So that agents can only perform their intended operations

  Background:
    Given I am signed in to the web interface as the admin
    And the repo "bobcob7/doc-server" is enrolled with target branch "main"

  @wip
  Scenario: Built-in roles ship with sane defaults
    When I list roles
    Then a built-in "author" role and a built-in "reviewer" role exist

  @wip
  Scenario: A role's instructions reach its agents
    Given the "reviewer" role has instructions configured
    When a "reviewer" agent asks for its instructions
    Then it receives the reviewer instructions
    And only the commands its role permits

  @wip
  Scenario: An author may not submit a verdict
    Given I am an agent with the "author" role
    When I try to submit a verdict
    Then the operation is denied

  @wip
  Scenario: A reviewer may not start a work branch or push
    Given I am an agent with the "reviewer" role
    When I try to start a work branch
    Then the operation is denied
    When I try to push
    Then the operation is denied

  @wip
  Scenario: A reviewer may clone the repo
    Given I am an agent with the "reviewer" role
    When I clone "bobcob7/doc-server"
    Then the clone succeeds

  @wip
  Scenario: Updating a role changes what its agents may do
    Given a custom role "release-captain" without the push operation
    When I grant it the push operation
    Then agents with that role may push

  @wip
  Scenario: Built-in roles cannot be deleted
    When I try to delete the built-in "author" role
    Then the deletion is rejected

  @wip
  Scenario: In the MVP, an agent's role is trusted from its environment
    Given an agent presenting the role "reviewer" in its environment
    Then the server treats it as a reviewer without further authentication
