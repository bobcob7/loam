# Work Spaces

This is a centralized code repo with MCP services.

### Purpose

I find it cumbersome to use GitHub or other centralized code repository systems.
Github has a lot of features that seem more like metadata about the content, not enriching the metadata.

### MCP

Local MCP CLI tool that setups up the Git repo and enables consistent workflow and querying and enforces authorization.

TODO

#### Graph DB

Code is digested upon merge to specific branches. AST is run on the code to find and record both dependencies and ref history.
This information can be queried through the CLI tool.

TODO

#### RAG

Documentation in the code, and the code itself is chunked and ingested into a vector DB upon merge to specific branches.
This information can be queried through the CLI tool.

TODO

#### Local PRs

TODO

##### List
Using the CLI tool, agents will be able to list reviewable PRs.

TODO

##### Get Details
Another command will pull the details of the PR.

TODO

##### Comment
Another command will pull individual PR comments.

TODO

##### Add Comment
Another command will either reply to a PR comment, or create a new one.

TODO

### Git

The CLI tool can be used to clone in a repo from the main server with the main server as it's only remote.

TODO

#### Enrollment

In the web interface. The admin should be able to enter in a GH PAT that enables cloning repos.
The admin should also be able to enroll repos, which will clone it to the server and periodically keep it up to date.

TODO

#### Clone

The CLI tool will have a git clone command that will clone in the repo at specific branch if specified.

TODO

#### Local PRs

The CLI tool can be used to create PRs from specific branches to target branches.
When a PR is opened, a title, and description is required.
Upon opening a PR, open PRs will show up on the CLI tool
A JSON schema can be setup by the admin from the web interface to enforce PR description format and comment/response formats.

TODO

#### Releases

TODO

### Web Interface

#### Auth

The interface is never intended for agents to access.
The admin will provide username and password on server startup.
These credentials will use basic HTTP Auth.

#### Repos

##### Auth

Admin will be able to set Github PAT or generate SSH key pair and give a public key to install upstream.

##### Enroll

The admin should be able to enroll repos, by upstream URL.

Target branches can be specified. These branches are eligable for PR creation.

### Workflow

#### Enroll Repo
1. Admin logs into web interface
2. Admin adds GH PAT. PAT is confirmed to work and that it has the appropriate permissions.
3. Admin types in exact repo name <group>/<repo_name>.
4. Server clones the repo locally.
5. Admin specifies target branches.
6. Server ingests data from the target branches.

#### Feature Work
1. Feature is defined by user.
2. Agent will use CLI to start a feature branch from target branch.
3. Agent will use CLI Clone the repo with the feature branch.
4. Agent will do work and make commits and push them.
5. Agent will use CLI to make open a PR and requests reviews.
6. Other agents review the PR and add comments.
7. Agent will read PR comments and iterate on them. Comments will be resolved once the agent thinks that they've been.
8. Once agent is satistied with the review, it's marked as complete. The web interface will show the propososed upstream PR title and description.
9. Admin will review the proposed upstream PR in the web interface. Admin can either accept or leave a comment.
10. Upon acceptance, a GH PR is created upstream with the proposed title and description.
