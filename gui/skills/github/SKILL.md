---
name: github
description: Work with GitHub - repositories, code, issues, pull requests, Actions, releases, and search - using the gh CLI and git.
---
# GitHub

Use the `gh` CLI; it is authenticated if `gh auth status` succeeds (GH_TOKEN from Settings also works). If gh is missing or logged out, ask the user to install it or run `gh auth login` in a terminal.

## Reading (always fine)
- Repos: `gh repo view OWNER/REPO`, `gh api repos/OWNER/REPO/contents/PATH --jq .content | base64 -d`
- Issues and PRs: `gh issue list -R OWNER/REPO`, `gh issue view N -R OWNER/REPO --comments`, `gh pr view N -R OWNER/REPO --comments`, `gh pr diff N -R OWNER/REPO`, `gh pr checks N -R OWNER/REPO`
- CI: `gh run list -R OWNER/REPO`, `gh run view ID -R OWNER/REPO --log-failed`
- Search: `gh search repos QUERY`, `gh search code QUERY`, `gh search issues QUERY`, `gh search prs QUERY`
- GraphQL and anything else: `gh api graphql -f query='...'`
- Local work: `gh repo clone OWNER/REPO -- --depth 1` into the workspace.
- Add `--json field1,field2 --jq ...` for compact output.

## Writing (confirm first)
Before anything that changes GitHub state (pushing, creating or closing issues/PRs, commenting, reviewing, releases, repo settings), show the user exactly what you will do and wait for approval, unless they already asked for that exact action.
- Work on a branch; never force-push or rewrite shared history unless asked.
- Never print tokens (`gh auth token`) or write them into files.
