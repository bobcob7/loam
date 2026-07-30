Feature: Code ingestion
  As an admin
  I want enrolled repos indexed and kept current automatically
  So that agents' graph and search queries reflect the real code

  Background:
    Given I am signed in to the web interface as the admin
    And the repo "bobcob7/doc-server" is enrolled with target branch "main"

  Scenario: Enrolling a repo ingests its indexed branch
    When enrollment completes
    Then the indexed branch "main" is ingested
    And graph and search queries return results for it

  Scenario: Advancing the target branch refreshes the index
    Given "main" has been ingested
    When "main" advances with a commit that adds "Logout" and removes "LegacyLogin"
    Then after ingestion a graph query finds "Logout"
    And a graph query no longer finds "LegacyLogin"

  @wip
  Scenario: Edges reflect the current code even in unchanged files
    Given file "handler.go" references "Login" defined in "auth.go"
    And only "auth.go" changes to rename "Login" to "Authenticate"
    When "main" advances and is ingested
    Then a graph query for references to "Authenticate" includes "handler.go"
    And "Login" is no longer found

  Scenario: The admin can force a reindex
    When I reindex "bobcob7/doc-server"
    Then a full ingest job runs for it
    And once it succeeds, queries reflect the current indexed branch

  Scenario: Viewing ingest job activity
    Given ingest jobs have run for enrolled repos
    When I open the Jobs view
    Then I see each job's repo, status, and timing

  Scenario: A failed ingest keeps the previous index
    Given "main" has been ingested successfully
    When the next ingestion fails
    Then the job is shown as failed with its error
    And graph and search queries still return the previous index
    And the reported ingested commit is unchanged
    And the job is retried
