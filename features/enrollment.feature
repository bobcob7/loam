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

  Scenario: The indexed branch must be a target branch
    Given "bobcob7/doc-server" is enrolled with target branch "main"
    When I try to designate "docs" as the indexed branch
    Then the change is rejected

  Scenario: Changing the indexed branch triggers a full ingest
    Given "bobcob7/doc-server" is enrolled with target branches "main" and "release" and indexed branch "main"
    When I change the indexed branch to "release"
    Then a full ingest job runs for "release"
    And once it succeeds, graph and search queries reflect "release"

  Scenario: Removing a target branch does not end work in flight
    Given a work branch targeting "release" is under review
    When I remove "release" from the target branches
    Then no new work branches can start from "release"
    But the existing work branch keeps its lifecycle

  Scenario: Enrolled repos report sync status
    Given "bobcob7/doc-server" is enrolled
    When I view the repo
    Then it shows a sync state and the time of the last successful sync

  Scenario: Removing a repo drops its data
    Given "bobcob7/doc-server" is enrolled
    And it has no open work branches
    When I remove it
    Then it is no longer enrolled
    And its mirror, graph, and vector data are dropped
    And its work branch history is gone

  Scenario: Removal is blocked by open work branches
    Given "bobcob7/doc-server" is enrolled
    And a work branch on it is in state "reviewable"
    When I try to remove it
    Then the removal is rejected as a failed precondition
    And I am told exactly which work branches block the removal
