## Things to remember
- If you hit a wall that can reasonably easily be solved by a human, stop and inform them.
- If you're unsure about a decision, or need more information, stop and ask.
- **When skipping a verification step** (e.g. tests that can't run in this environment): always post a specific, actionable explanation — name the exact constraint (e.g. "Docker daemon can't start: iptables unavailable in nested container") and what the human needs to do to unblock it. Never silently skip without explaining why.
- Don't use `<` or `>` symbols for placeholders in markdown documents, or anything posted to GitHub (bodies, comments), as these render as tags. If you need a literal `<` or `>` symbol otherwise, use `&gt;` or `&lt;` instead, or wrap them in backticks.
- Don't put line breaks within individual paragraph in markdown documents, or anything posted to GitHub (bodies, comments). These don't render well in some markdown renderes, such as GitHub issues.

## Tech Stack
- Go 1.26, Hatchet (workflow orchestration; backed by PostgreSQL via the Hatchet server — the Go code does not connect to Postgres directly)
- CGO + FFmpeg 8 shared libraries (media processing via `github.com/asticode/go-astiav`)
- `golangci-lint` for static analysis
- Nix (flake.nix) for reproducible dev environments

## Project Structure
```
media-processor/
├── CLAUDE.md
├── SPEC.md
├── README.md
├── flake.nix                  # Nix dev environment
├── Makefile
├── go.mod
├── .golangci.yml
├── docker-compose.yml         # Local Hatchet dev stack
├── .claude/
│   ├── commands/              # Slash commands
│   └── tasks/                 # Per-issue working files (gitignored)
├── bin/                       # Build outputs (gitignored)
├── cmd/
│   ├── watcher/               # Cron-driven directory scanner + Hatchet job submission binary
│   ├── worker/                # Hatchet worker + workflow handlers binary
│   └── gen-config-schema/     # Emits the watcher YAML JSON Schema (see schemas/)
├── docs/                      # Operator-facing documentation (configuration, hardware accel, metrics)
├── e2e/                       # End-to-end test suite (Docker-based, build tag `e2e`)
├── internal/
│   └── watcherconfig/         # Shared watcher config types + validation
├── pkg/
│   ├── ffmpeg/                # In-process FFmpeg wrapper (CGO via go-astiav)
│   ├── ffprobe/               # In-process ffprobe-equivalent inspector (CGO via go-astiav)
│   ├── logging/               # zerolog-backed slog setup
│   ├── medialib/              # MediaType/Movie/Episode domain types + radarr/sonarr API clients
│   ├── metrics/               # Prometheus + OTLP metrics provider
│   └── webhook/               # Outbound failure-notification HTTP client
├── schemas/                   # Generated JSON schema(s) for config files
├── workflows/                 # Hatchet workflow definitions and step handlers
│   ├── placeholder.go         # No-op standalone task registered with the worker to verify Hatchet connectivity
│   ├── media/                 # Top-level media workflow
│   └── steps/                 # Individual workflow step handlers (probe, detectcrop, transcode, etc.)
└── deploy/                    # Deployment configs (Helm charts)
```

## Commands
- Build: `make build` (outputs `bin/watcher` and `bin/worker`)
- Format: `make fmt`
- Vet: `make vet`
- Lint: `make lint` (or `make lint-fix` to auto-apply fixes)
- Unit tests: `make test`
- Integration tests: `make test-integration` (starts a local Hatchet via `make hatchet-up` and runs `-tags=integration`)
- E2E tests: `make test-e2e` (requires Docker; first run downloads ~700 MB BBB fixture)
- Benchmarks: `make benchmark`
- Generate watcher JSON schema: `make generate-schema` (writes `schemas/watcher.schema.json`)
- Update Go modules + sync Hatchet image tags: `make update-dependencies`
- Local Hatchet dev stack: `make hatchet-up` / `make hatchet-down` / `make hatchet-token`

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
- Loop iteration variable names must be the full singular form of the collection name. For example, use `service` when ranging over `services`, `entry` when ranging over `entries`, `category` when ranging over `categories`. Single-letter names (`s`, `v`, `f`, `k`) and shortened names (`cat` for `category`, `svc` for `service`) are not allowed unless the collection itself uses that short name (e.g. `cat` is fine when ranging over `cats`).

## Documentation

- When a change affects any user-facing surface — environment variables, configuration fields, CLI flags, Prometheus metrics, webhook payloads, or operator-visible behavior — update the relevant file(s) in `docs/` as part of the same PR.
- User-facing surfaces and their primary doc files: configuration options → `docs/configuration.md`; hardware acceleration → `docs/hardware-acceleration.md`; metrics → `docs/metrics.md`.
- If no existing doc file covers the changed surface, create one under `docs/` and link it from `README.md`.

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
