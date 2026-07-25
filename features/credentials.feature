Feature: Managing upstream credentials
  As an admin
  I want to configure a token per forge host
  So that the server can clone, sync, and open PRs upstream

  Background:
    Given I am signed in to the web interface as the admin

  Scenario: Setting a token for a forge host
    When I set an upstream token for "github.com"
    Then the server validates the token
    And the credential status for "github.com" shows a token is present

  Scenario: A rejected token is reported
    When I set an invalid upstream token for "github.com"
    Then the credential is rejected as invalid

  Scenario: One token covers REST and git
    Given a credential exists for "github.com"
    When I enroll "https://github.com/bobcob7/doc-server"
    Then the server proves git read and write access with the token before cloning

  Scenario: A token without git access fails enrollment
    Given a credential exists for "github.com" whose token lacks git access
    When I enroll "https://github.com/bobcob7/doc-server"
    Then the enrollment is rejected because the token cannot access the repo over git

  Scenario: Credentials are shared by all repos on a host
    Given a credential exists for "github.com"
    When I enroll two repos hosted on "github.com"
    Then both use the "github.com" credential

  Scenario: Credentials are scoped per host
    Given a credential exists for "github.com"
    When I view the credential status for "forgejo.example.com"
    Then it shows no credential is present
