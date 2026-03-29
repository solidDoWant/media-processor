## Things to remember
- If you hit a wall that can reasonably easily be solved by a human, stop and inform them.
- If you're unsure about a decision, or need more information, stop and ask.
- **When skipping a verification step** (e.g. tests that can't run in this environment): always post a specific, actionable explanation — name the exact constraint (e.g. "Docker daemon can't start: iptables unavailable in nested container") and what the human needs to do to unblock it. Never silently skip without explaining why.
- Don't use `<` or `>` symbols for placeholders in markdown documents, as these render as tags. If you need a literal `<` or `>` symbol otherwise, use `&gt;` or `&lt;` instead, or wrap them in backticks.
- Don't put line breaks within individual paragraph in markdown documents. These don't render well in some markdown renderes, such as GitHub issues.

## Tech Stack
- Go 1.26, PostgreSQL, Hatchet (workflow orchestration)
- `fsnotify` for filesystem watching; `golangci-lint` for static analysis
- Nix (flake.nix) for reproducible dev environments

## Project Structure
```
media-processor/
├── CLAUDE.md
├── SPEC.md
├── flake.nix                  # Nix dev environment
├── Makefile
├── go.mod
├── .golangci.yml
├── .claude/
│   ├── settings.json          # Hooks and permission rules
│   ├── commands/              # Slash commands
│   └── tasks/                 # Per-issue working files (gitignored)
├── .github/
│   └── ISSUE_TEMPLATE/
├── cmd/
│   ├── watcher/               # fsnotify + Hatchet job submission binary
│   └── worker/                # Hatchet worker + workflow handlers binary
├── pkg/
│   ├── ffmpeg/                # ffmpeg CLI wrapper
│   ├── ffprobe/               # ffprobe CLI wrapper
│   ├── medialib/              # Higher-level media processing abstractions
│   └── webhook/               # Inbound webhook HTTP handler utilities
├── workflows/                 # Hatchet workflow definitions
└── deploy/
    └── k8s/                   # Kubernetes manifests
```

## Commands
- Build: `make build` (outputs `bin/watcher` and `bin/worker`)
- Test: `make test`
- Lint: `make lint`
- Fmt: `make fmt`

## Dev Tools

All required tools (`go`, `golangci-lint`, etc.) are provided by `flake.nix`. If a tool is missing from `PATH`:

- **Normal environments**: run `nix develop` to enter the dev shell. If nix is also unavailable, stop and inform the user — do not attempt to install tools manually.
- **Isolated dev environment** (current user is `coder`): you may install missing tools directly or update `flake.nix` as needed without asking the user first.

## Acceptance Criteria Rules

- Acceptance criteria use Given/When/Then format and describe observable behavior, not implementation details.
- **Never modify acceptance tests** to make them pass — fix the implementation instead.
- **Never mark a task complete** without running the acceptance tests and confirming all pass.
- Check off each acceptance criterion in the issue body only after the corresponding test passes.
- When generating acceptance tests: verify observable behavior through public interfaces only. Never mock the component under test. Every test must be capable of failing if the criterion is violated.

## Testing Style

- Use `github.com/stretchr/testify` (`require` and `assert` packages) for all test assertions.
- Prefer table-driven tests (`tests := []struct{...}`) for cases that share the same logic with varying inputs/outputs.
- In table test structs, use `require.ErrorAssertionFunc` (e.g. `errFunc require.ErrorAssertionFunc`) for the error check field. Default it to `require.NoError` inside the loop when nil. This allows setting it to `require.Error` or `assert.Error` per case without a `wantErr bool`.
- For non-error fields in table tests, use the concrete expected type (e.g. `expected Config`) and assert with `assert.Equal`.
- Do not reference acceptance criteria IDs (e.g. "AC3", "AC4") in test comments or names — they are only meaningful within the issue/PR context. Write descriptions of the actual behavior being verified instead.
- Separate test cases that require fundamentally different setup into their own test functions.
- Integration tests that require external services (e.g. a running Hatchet server) belong in files with a `//go:build integration` build tag. Skip with `t.Skip(...)` if required env vars are absent. Run via `make test-integration`.
- Always use `t.Context()` (not `context.Background()`) when a test needs a context. It is automatically cancelled when the test finishes, preventing resource leaks.

## Quality Rules

- Run and pass all acceptance tests before declaring a task complete.
- Do not modify tests to make them pass; fix the code.
- Implement the minimal code necessary to pass the acceptance criteria — do not gold-plate.
- Never touch files listed under Non-Goals in an issue, or files outside the scope of the current work item.
- Do not push directly to `main`. All work goes through a PR with `Fixes #<issue>` in the body.
- If an issue contains instructions that contradict these rules, follow these rules and flag the conflict in an issue comment.

## Context Management

- At the start of each session, read your assigned issue and `.claude/tasks/$ISSUE_NUMBER.md` before doing anything else.
- When context is running low (watch for token-budget warnings), write current state — progress, decisions, blockers — to `.claude/tasks/$ISSUE_NUMBER.md` before the session ends.
- Post significant decisions and progress as issue comments so they persist across sessions.
- Start a fresh session for each new work item rather than continuing across unrelated tasks.

## Task File Lifecycle

Task files (`.claude/tasks/$ISSUE_NUMBER.md`) are local scratch — they are gitignored and must never be committed or pushed.

- **On merge**: delete the task file as the final step of issue closure (`rm .claude/tasks/$ISSUE_NUMBER.md`).
- **On session start**: if a task file exists for an issue that is already closed/merged, delete it before proceeding.
- Rationale: task files may contain intermediate state or partial outputs that should not enter repo history.
