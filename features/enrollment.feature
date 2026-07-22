Feature: Enrolling and managing repos
  As an admin
  I want to enroll upstream repos and configure them
  So that agents can work on them through Loam

  Background:
    Given I am signed in to the web interface as the admin
    And a working credential exists for the forge host "github.com"

  Scenario: Enrolling a repo by upstream URL
    When I enroll "https://github.com/bobcob7/doc-server" with target branch "main"
    Then the repo "bobcob7/doc-server" is enrolled
    And the server clones it and begins syncing and ingesting "main"

  Scenario: The repo identifier is derived from the URL
    When I enroll "https://github.com/bobcob7/doc-server" with target branch "main"
    Then its identifier is "bobcob7/doc-server"

  Scenario: Updating the target branches
    Given "bobcob7/doc-server" is enrolled with target branch "main"
    When I set its target branches to "main" and "release"
    Then both "main" and "release" are eligible as work-branch targets

  Scenario: Registering a description schema validates descriptions
    Given "bobcob7/doc-server" is enrolled
    When I register a description JSON schema for it
    Then opening a work branch with a non-conforming description is rejected
    And opening one with a conforming description succeeds

  Scenario: Enrolled repos report sync status
    Given "bobcob7/doc-server" is enrolled
    When I view the repo
    Then it shows a sync state and the time of the last successful sync

  Scenario: Removing a repo drops its data
    Given "bobcob7/doc-server" is enrolled
    When I remove it
    Then it is no longer enrolled
    And its mirror, graph, and vector data are dropped
