Feature: Agent orientation
  As an agent
  I want to learn how to use Loam and what my role permits
  So that I can start working correctly without external documentation

  Background:
    Given I am the agent "grace-hopper-3-author" with the "author" role

  Scenario: Instructions orient a new agent
    When I ask for instructions
    Then I receive general usage and conventions
    And the commands available to my role
    And the instructions configured for my role

  Scenario: The command list is filtered to my role
    When I ask for instructions
    Then commands my role cannot perform are not listed

  Scenario: Help for a single command
    When I ask for instructions for one command
    Then I receive only that command's usage

  Scenario: whoami reports my identity
    When I ask who I am
    Then I am told my name, id, role, and full identifier

  Scenario: whoami works without contacting the server
    Given the server is unreachable
    When I ask who I am
    Then I still get my identity from the environment

  Scenario: whoami --verify reports a role the server does not recognize
    Given I am the agent "grace-hopper-3-ghost" with the "ghost" role
    When I ask to verify who I am
    Then the verification is rejected as unauthorized, naming the role
