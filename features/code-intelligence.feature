Feature: Querying code intelligence
  As an agent
  I want to query a repo's structure and search its docs and code
  So that I can understand the codebase without reading the whole tree

  Background:
    Given the repo "bobcob7/doc-server" is enrolled with target branch "main"
    And its target branch has been ingested
    And I am working inside a clone of "bobcob7/doc-server"

  Scenario: Finding where a symbol is defined
    When I ask the graph where "Login" is defined
    Then I get the file and line of its definition

  Scenario: Finding references to a symbol
    When I ask the graph for references to "Login"
    Then I get every location that references it

  Scenario: Finding what depends on a target
    When I ask the graph for the dependents of "auth.go"
    Then I get the code that would be affected by changing it

  Scenario: Semantic search returns relevant chunks with provenance
    When I search for "how is authentication handled"
    Then I get the most relevant doc and code chunks
    And each result names its repo, file, and line range

  Scenario: Queries default to the current repo
    When I run a query without specifying a scope
    Then it is scoped to "bobcob7/doc-server"

  Scenario: Searching across all repos
    When I search across all enrolled repos
    Then results may come from any enrolled repo

  Scenario: Graph fan-out does not link repos in the MVP
    When I run a graph query across all enrolled repos
    Then each repo's results are returned independently
    And a usage in one repo is not linked to a definition in another

  Scenario: A query without a resolvable scope is rejected
    Given I am not inside a repo directory
    When I run a query without specifying a scope
    Then the query is rejected as a usage error
