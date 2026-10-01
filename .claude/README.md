# .claude/

Repo-level configuration for Claude Code.

## Files

- `settings.json` — pre-approves permissions for the tools this project uses (`make`, `go test`, `sqlc`, `migrate`, `mockgen`, read-only git, file readers/searchers). This lets the verify loop (`make lint && make test`) run without permission prompts.
- `skills/` — project-specific skills loaded on demand:
  - `new-feature/` — checklist + scaffolder reference for adding a feature
  - `go-hex-antipatterns/` — BAD/GOOD examples for review/refactor
  - `go-hex-recipes/` — recipes for endpoint/dep/cross-feature changes
  - `ui-kb-validate/` — static checks plus a live `/chat` run of the operational eval
  - `ui-kb-eng-validate/` — drives the MCP stdio server with the engineering eval,
    the path a coding agent actually takes
  - `ui-kb-merge-import/` — merges per-source KB bundles, distils, and imports the corpus
  - `ui-kb-reference-bundle/` — turns hand-written markdown into a `reference` bundle
  - `ui-kb-requirement-validate/` — requirement-acceptance questions against live `/chat`

`settings.local.json` is per-machine and stays out of version control.

## settings.json: scope of the allowlist

The allowlist is **permissive for dev-loop convenience**, not a security boundary. Entries like `Bash(make:*)` and `Bash(go run:*)` allow arbitrary execution — `make some-shell-target` or `go run ./scripts/anything.go` will run without prompting.

This is intentional: in a developer's own workstation environment, the friction of approving every `make test` is worse than the marginal safety of stopping it. Destructive operations (`rm -rf`, `git push --force`, `git reset --hard`) are explicitly denied to keep the obvious foot-guns gated.

If you adopt this template in a context where the model is running in a less-trusted environment (CI on untrusted PRs, shared sandboxes, etc.), tighten the allow list — replace `Bash(make:*)` with a fixed set of `Bash(make lint)`, `Bash(make test)`, etc.

## Skills won't load automatically — they fire on relevance

The skill `description` field decides whether Claude reaches for it. If you find Claude isn't using a skill you expected, check whether your prompt matches the wording in the description; tighten or broaden it as needed.
