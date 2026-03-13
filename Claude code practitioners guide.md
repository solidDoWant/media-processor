# Building serious software with Claude Code: a practitioner's guide

Claude Code is most effective when treated as a fast but amnesiac junior engineer — one who needs clear specs, tight guardrails, and constant verification, but who can execute at extraordinary speed when properly directed. The single highest-leverage investment is not prompting technique but *context engineering*: the quality of your project documentation, CLAUDE.md files, specs, and architecture docs determines output quality far more than model choice or clever prompting. Practitioners who invest heavily upfront report **2–15x productivity gains**, while those who skip structure produce code that's 95% garbage on first attempt. This guide synthesizes official Anthropic documentation, community consensus from dozens of practitioner reports, and hard-won lessons from staff-level engineers who've shipped production systems with Claude Code through early 2026.

---

## 1. Plan the architecture before touching code

Every experienced Claude Code practitioner converges on the same lesson: upfront planning is the highest-ROI activity. The pattern that works is Explore → Plan → Document → Implement, with clear gates between phases.

### Write a machine-actionable spec

Your project spec (`SPEC.md`) must be written so an AI agent can start work without asking clarifying questions. Instead of "we need an API endpoint," write: "Create a POST endpoint at `/api/v1/notifications/preferences` that accepts `{user_id: string, channels: string[]}` and returns `{updated: boolean}`." Treat the spec as a living contract that governs every agent session.

A good spec covers six areas: **executable commands** (with exact flags), **testing expectations** (framework and coverage targets), **project structure** (explicit directory map), **code style** (one real snippet beats three paragraphs), **git workflow** (branch naming, commit format), and **boundaries** (what the agent must never touch — secrets, vendor directories, production configs).

### Use Plan Mode before execution

Start every non-trivial task in Plan Mode (`Shift+Tab` twice, or `--permission-mode plan`). Claude reads files, answers questions, and proposes approaches — but makes no changes. Point it at relevant code: "Read `/src/auth` and understand how we handle sessions." Ask for 2–3 alternative approaches. Use keywords like "think hard" or "ultrathink" to trigger deeper reasoning.

Once you approve the plan, **save it to a file** — not just chat history. Chat history doesn't survive context compaction.

---

## 2. Work items: GitHub Issues as the coordination layer

GitHub Issues is the right place to track work items — not files inside the repository. Issues provide cross-service visibility, branch-independent persistence, PR integration, and team collaboration that in-repo files cannot match. For a multi-service project, this is decisive: a shared library change affecting three services is naturally represented as a parent issue with service-scoped sub-issues, all visible in a single board.

That said, **pure issue-only approaches underperform hybrids**. The most effective pattern is to use Issues for the *what* (assignment, acceptance criteria, status, history) and lightweight in-repo files for the *how* (working context the agent needs within and across sessions). Both layers are described below.

### GitHub Issues: the assignment and tracking layer

**Sub-issues** (generally available April 2025) support up to 8 levels of nesting and 100 children per parent, with automatic progress tracking on parent issues. This maps naturally to work decomposition: a parent issue "Add user authentication" can contain sub-issues for "OAuth2 provider integration," "Login page UI," "Session management middleware," and "Auth service API endpoints," each scoped to a different service. Cross-repository relationships work within the same organization.

**Issue types** (Bug, Feature, Task, Epic — customizable per org) and **issue dependencies** (blocking/blocked-by relationships, GA August 2025) complement sub-issues to provide classification and ordering.

**Label taxonomy** for multi-service projects should use two dimensions: service labels (`service:auth`, `service:api`, `lib:shared`) and status labels (`status:todo`, `status:in-progress`, `status:blocked`, `status:done`). These are queryable by agents and visible to the team in the same view.

### Structuring issues so agents can consume them

The convergent best practice is a structured issue template with these sections:

**Title**: Use conventional-commit-style prefixes — `feat(auth): Add OAuth2 login flow` or `fix(api): Handle empty payment response`. This gives agents immediate scope and type information without reading the body.

**Body**:

```markdown
## Description
Brief outcome-focused context. No implementation details.

## Acceptance Criteria
- [ ] User can log in with a Google account
- [ ] Login with wrong password shows "Access denied" message
- [ ] Session expires after 24 hours of inactivity

## Technical Context
- Relevant files: `src/auth/oauth.ts`, `services/api/handlers/login.go`
- Depends on: #89 (auth service API endpoints) must be merged first
- Affected services: web-app, auth-service

## Non-Goals
- Password reset flow (tracked separately in #112)
- 2FA support (out of scope for this milestone)
```

Acceptance criteria must be **observable behaviors in checkbox form**, not implementation details. "User can log in with a Google account" is correct; "OAuth2 token exchange succeeds" is not. Non-Goals are as important as goals — they prevent agents from gold-plating.

GitHub's YAML issue forms (`.github/ISSUE_TEMPLATE/*.yml`) enforce this structure at creation time. Create separate templates per service type (`feature-auth-service.yml`, `feature-web-app.yml`) with pre-populated service labels.

**Issue sizing**: one issue = one PR. If an issue would touch more than roughly 10 files, decompose it into sub-issues. Agents perform measurably better on atomic, well-scoped tasks.

### In-repo working context: the agent's operational layer

Agents cannot rely on Issues alone — Issues don't contain the technical working context needed to operate within the codebase. For this, use a small set of in-repo files that the agent reads at the start of each session.

**CLAUDE.md** files (covered in section 4) provide project-wide and per-service conventions. These are the most important in-repo files.

**Per-work-item context files** are lightweight working memory for the agent, gitignored and ephemeral. They live in a `tasks/` directory (or `.claude/tasks/`, depending on preference) and are named by issue number:

```
.claude/
└── tasks/
    └── 123.md    # Working notes for issue #123 (gitignored)
```

A task working file is not documentation — it's an operational scaffold:

```markdown
# Task #123: Login page

## Reading list (in order)
- src/auth/oauth.ts (existing OAuth helpers)
- src/pages/Register.tsx (reference for page structure)
- services/api/handlers/auth.go:45 (the endpoint this page calls)

## Key constraints
- Must reuse the existing <FormField> component
- Auth token stored in httpOnly cookie, not localStorage
- Error messages must match the copy in public/i18n/en.json

## Subtasks
- [ ] Scaffold component
- [ ] Wire up form submission
- [ ] Handle error states
- [ ] Acceptance tests pass
```

These files are cheap to create and discard. When work on an issue completes, the file is deleted. When an issue is reopened, a new one is created from the current state of the codebase.

### The agent workflow: issue to merged PR

1. **Context loading**: agent runs `gh issue view 123 --json title,body,labels,comments` and reads its task working file (`cat .claude/tasks/123.md`).
2. **Self-assignment and status update**: `gh issue edit 123 --add-label "status:in-progress" --remove-label "status:todo"`.
3. **Branch creation**: `gh issue develop 123 -c --base main` creates a branch linked to the issue. Convention: `feat/auth-oauth-123`.
4. **Plan posting**: agent posts its implementation plan as an issue comment before coding, creating an audit trail and giving humans a chance to intervene.
5. **Implementation**: agent works through the task checklist, updating checkboxes in the issue body as each is completed.
6. **PR creation**: `gh pr create -t "feat(auth): Add OAuth login" -b "Fixes #123"`. The `Fixes #123` keyword auto-closes the issue on merge.

Codify this workflow as a slash command in `.claude/commands/`:

```markdown
# .claude/commands/start-issue.md
Read the GitHub issue number provided. Use `gh issue view` to load it fully.
Read `.claude/tasks/$ISSUE_NUMBER.md` if it exists. Self-assign the issue and 
move it to in-progress. Create a linked branch. Post a brief implementation 
plan as a comment. Begin work on the acceptance criteria.
```

### The `gh` CLI vs. the GitHub MCP server

Anthropic's official best practices recommend the **`gh` CLI** as the primary interface — it requires no additional setup and works identically in local and CI environments. Agents can run `gh issue view`, `gh issue create`, `gh issue list`, and `gh issue edit` directly as shell commands.

The **GitHub MCP server** (`github/github-mcp-server`) adds value for richer programmatic access: sub-issue manipulation, project board management, and structured search beyond what the CLI offers. Its built-in content sanitization strips hidden Unicode, unsafe HTML, and hidden markdown from issue content — important protection against prompt injection from arbitrary issue text. Configure it via remote server to avoid local binary management:

```bash
claude mcp add-json github '{"type":"http","url":"https://api.githubcopilot.com/mcp/","headers":{"Authorization":"Bearer YOUR_PAT"}}'
```

Restrict toolsets to reduce context overhead: `"X-MCP-Toolsets":"repos,issues"`. Read-only mode (`"X-MCP-Readonly":"true"`) is available for agents that should only consume issues, not create them.

One practical limitation: raw `gh` CLI interactions cost roughly **73% more tool calls** than file reads due to help-text lookups, schema probing, and output parsing. For agents running many short issue interactions in a session, this is worth accounting for in context budgets.

---

## 3. Acceptance criteria-driven development

You should not need to write tests yourself. Write acceptance criteria in plain language, and let the agent both generate the tests and implement the code to pass them. This is **Acceptance Test Driven Development (ATDD)** — a methodology with growing Claude Code ecosystem support as of 2025–2026, most notably in the `atdd` Claude Code plugin inspired by Robert C. Martin's work.

### How it works

When using GitHub Issues, acceptance criteria live in the issue body as checkboxes. For agents running in isolated environments, they may also be mirrored in an `ACCEPTANCE.md` file in the task working directory to avoid repeated API calls. The format is Given/When/Then:

```
GIVEN no session is active
WHEN a user submits the login form with correct credentials
THEN the user is redirected to the dashboard
THEN a valid session token is stored

GIVEN no session is active
WHEN a user submits with an incorrect password
THEN the login page remains visible
THEN the message "Access denied" is displayed
THEN no session token is stored
```

The critical rule, borrowed from ATDD doctrine: write criteria in **domain language**, not implementation language. Do not write "the `AuthController.login()` method returns a 401." Write "the user sees 'Access denied'." Criteria describe *what the system does*, not *how it does it*.

You then prompt Claude: "Read the acceptance criteria from issue #123. Generate failing acceptance tests in `tests/acceptance/login.spec.ts` using our existing test framework (see CLAUDE.md for conventions). Then implement the login page to make all tests pass. Do not modify the acceptance tests."

### Why this is better than writing tests yourself

You maintain authority over *what* the system does; Claude decides *how* to verify and implement it. Tests are regeneratable from unchanged criteria if you refactor the test framework. Criteria are readable by anyone without reading test code. Domain-language criteria are much harder to game than implementation-level unit tests — a test that verifies "Access denied is displayed" cannot be faked by returning a hardcoded string.

### Preventing test gaming

Even with domain-language criteria, agents can still produce weak tests. Common failure modes: tests that check presence of a `<div>` rather than actual error state, mocks that bypass the behavior being tested, or acceptance tests subtly weaker than the criteria.

Mitigations: use a **separate verification subagent** — after implementation, spawn a fresh agent with only the acceptance criteria and ask "Review the acceptance tests in `tests/acceptance/login.spec.ts`. Do they faithfully implement the criteria? List any gaps." Some teams configure a `Stop` hook that automatically runs mutation testing (Stryker, mutmut, PIT) after implementation.

Add to CLAUDE.md: "Acceptance tests must verify observable behavior through public interfaces only. Never mock the component under test. Every test must be capable of failing if the criterion is violated."

### EARS-style criteria for edge cases

For complex work items, supplement Given/When/Then with EARS-style requirements for non-functional and edge-case behaviors:

```
WHILE a login request is in flight, the submit button shall be disabled.
IF the auth service is unreachable, the system shall display a connection error and preserve the form state.
```

---

## 4. CLAUDE.md: your highest-leverage configuration

CLAUDE.md is not project documentation. It's a **context injection file** — every line competes for attention in a finite context window. Claude Code's system prompt already contains ~50 instructions, and frontier LLMs reliably follow roughly **150–200 total instructions**. As you add more, adherence to all of them degrades.

### What to include

Structure your CLAUDE.md around three pillars. **WHAT**: tech stack, project structure, a map of key directories. **WHY**: the purpose of the project and its components — enough for Claude to make architectural decisions that align with your intent. **HOW**: build, test, lint, and deploy commands with exact flags.

```markdown
## Tech Stack
- Go 1.22, gRPC, PostgreSQL 16, Redis
- Protobuf for service definitions, sqlc for queries

## Project Structure
- `pkg/lib/` – Shared library (imported by all services)
- `services/ingest/` – Data ingestion service
- `services/api/` – External API gateway

## Commands
- Build all: `make build`
- Test: `go test ./... -race -count=1`
- Lint: `golangci-lint run`
- Proto generation: `make proto`

## Work Items
- Work items are tracked as GitHub Issues
- Read your assigned issue with `gh issue view $ISSUE_NUMBER --json title,body,labels,comments`
- Load task context from `.claude/tasks/$ISSUE_NUMBER.md` if it exists
- Post implementation plan as issue comment before coding
- Check off acceptance criteria checkboxes as you complete them

## Quality Rules
- Never mark a task complete without running and passing its acceptance tests
- Acceptance tests must verify observable behavior through public interfaces only
- Never modify acceptance tests to make them pass
```

**Exclude** code style rules that a linter handles, instructions not applicable to every session, full code snippets (go stale; use `file:line` references), and comprehensive manuals. Document what Claude gets *wrong*, not everything it could possibly need. Keep total length **under 200 lines** per file, ideally under 100.

### Hierarchical CLAUDE.md for multi-service projects

For a monorepo with a shared library and multiple services, nest CLAUDE.md files:

```
project-root/
├── CLAUDE.md              # Universal: 80-120 lines, shared conventions
├── pkg/lib/
│   └── CLAUDE.md          # Library-specific: API contracts, versioning rules
├── services/ingest/
│   └── CLAUDE.md          # Ingest service: data formats, pipeline conventions
└── services/api/
    └── CLAUDE.md          # API service: endpoint patterns, auth flows
```

Claude reads these hierarchically: home → project root → nearest subdirectory to working files. Root CLAUDE.md handles universal rules; nested files handle module-specific gotchas.

---

## 5. Context windows and compaction

### The actual state of 1M context in Claude Code

Sonnet 4.6 supports a **200K context window by default**, with a **1M token context window in beta**. The 1M window requires the `context-1m-2025-08-07` API beta header, usage tier 4 or custom rate limits, and carries premium pricing (2x input, 1.5x output for tokens beyond 200K). In Claude Code's interactive interface, it's accessible by appending `[1m]` to the model name (`claude-sonnet-4-6[1m]`), but availability varies by subscription tier with some reported instability on the Max plan.

Practically: plan for **200K as your reliable baseline** in interactive sessions. 1M is available in API/headless workflows with appropriate access, at higher cost for large contexts.

### Context awareness changes how the model behaves

Sonnet 4.6 includes a **context awareness feature** that tracks remaining token budget and communicates it after each tool call:

```
<system_warning>Token usage: 120000/200000; 80000 remaining</system_warning>
```

The model uses this to persist on tasks rather than prematurely declaring completion — directly addressing the fake completion failure mode. It can also prompt the model to write state to files before context runs low.

### Compaction strategy

With 200K context: monitor with `/context`, compact at 50–60% to preserve working space, and use `/compact "Preserve only the auth module changes and test results"` for targeted preservation. Avoid relying on more than roughly two compaction cycles before starting a fresh session — context quality degrades even with compaction.

With 1M context (when available): longer sessions become practical without compaction. But the practice of externalizing state to task working files and GitHub Issues remains correct — these are handoff protocols between separate agent sessions, not just compaction workarounds.

**Server-side compaction** (currently beta on Opus 4.6) provides automatic server-side summarization for very long-running sessions.

---

## 6. Verification: hooks over prompt instructions

Prompt-level instructions and hooks serve different purposes and are not alternatives to each other — use both.

### The fundamental distinction

**CLAUDE.md instructions are advisory** — the model reads and attempts to follow them, but there is no enforcement mechanism. Attention degrades across long sessions. **Hooks are deterministic** — they execute as shell scripts on specific lifecycle events, regardless of what the model decides.

The practical principle: **use hooks for anything that must happen every time without exception; use prompt instructions for shaping judgment-based decisions**.

### PostToolUse hooks: continuous quality enforcement

Configure PostToolUse hooks to run formatters, linters, and type checkers after every file write. These fire automatically — no prompting needed, no forgetting:

```json
{
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "Write|Edit|MultiEdit",
        "hooks": [{
          "type": "command",
          "command": "FILE=$(jq -r '.tool_input.file_path // empty'); [ -n \"$FILE\" ] && gofmt -w \"$FILE\" 2>/dev/null; exit 0"
        }]
      }
    ]
  }
}
```

When hooks report errors, Claude Code surfaces them back to the agent as feedback. The agent reads the linter output and fixes the problem before proceeding. This creates an automatic correction loop that removes the need for "run `make lint` and fix any errors" in your task checklists.

### PreToolUse hooks: commit gates

Configure a PreToolUse hook on `git commit` to enforce test passage before any commit is allowed:

```json
{
  "PreToolUse": [
    {
      "matcher": "Bash(git commit*)",
      "hooks": [{
        "type": "command",
        "command": "make test && make lint || (echo 'Tests or lint failed. Fix before committing.' && exit 1)"
      }]
    }
  ]
}
```

When the hook exits non-zero, the commit is blocked and the error message feeds back to the agent. This is distinct from a git pre-commit hook (which governs human commits) — run both: git hooks protect you from yourself, Claude Code PreToolUse hooks protect you from the agent.

### Other useful hooks

**Stop hook**: run a final validation pass when Claude finishes a session — check acceptance tests, update issue status, generate a summary.

**PreCompact**: save critical working state before context compaction fires, so the next session can pick up cleanly.

### Practical cautions

Hooks run in a non-interactive shell without `.bashrc`/`.zshrc`. Use absolute paths for tools installed via version managers (`nvm`, `asdf`). Scope hooks precisely — a `Bash` matcher fires on every shell command including `ls`, creating severe performance overhead. Use specific command patterns. Hooks can timeout at 10 minutes; keep them fast or run expensive checks only on commit.

---

## 7. Execution environments and parallelism

### Separate environments per work item

Each work item should run in its own isolated execution environment — its own container, VM, or cloud sandbox. This gives stronger isolation than worktrees:

- No shared filesystem state between agents — one agent cannot clobber another's work-in-progress
- No shared runtime (ports, databases, caches) — each environment can spin up its own service dependencies
- Clean baseline by default — no leftover state from a previous session
- Parallelism without coordination overhead — no file-based locking needed

In this model, each agent session receives the repository (mounted or freshly cloned), the task working file (`.claude/tasks/123.md`) as its starting context, and any environment-specific configuration. When the session completes, the diff is extracted and reviewed.

GitHub Issues integrate naturally with this model: the agent reads its issue at startup regardless of which environment it runs in, and posts its plan and progress as comments that persist independently of any local environment.

### Worktrees for intra-session subagents

Git worktrees remain valuable for **subagent workflows within a single session**. When Claude Code spawns a subagent for a bounded task (codebase exploration, a specific refactor, research), a worktree gives it an isolated working directory without the overhead of a full environment. The subagent reports results to the orchestrating agent.

The practical rule: **worktrees for intra-session subagents; separate environments for inter-session parallelism**.

---

## 8. Prompting strategies

### Use distinct modes and never mix them

**Build mode**: concrete, action-oriented ("Add input validation to the contact form to check email format and required fields"). **Debug mode**: share logs, errors, and environment details. **Refine mode**: either critique OR rewrite, never both at once. **Learn mode**: ask teaching questions about the codebase.

Always include behavioral constraints on tests: "Run the acceptance tests for this work item and confirm all pass. If any fail, fix the implementation — not the tests." Reference technical terms precisely — "implement exponential backoff with jitter" produces better results than "add retry logic."

### Give context efficiently

Don't paste everything into the prompt. Let Claude fetch what it needs via the `gh` CLI, file reads, or MCP tools. For each work item session: "Run `gh issue view 123 --json title,body,labels,comments` and read `.claude/tasks/123.md`. Complete the work in the acceptance criteria. Update the issue checkboxes as you complete each one."

### One-shot vs. iterative sessions

Reserve one-shot mode (`claude -p`) for well-defined, bounded tasks: code review, generating a specific function, CI pipeline steps. For anything complex, use iterative sessions: plan → implement one piece → verify → commit → next piece.

---

## 9. Repository structure

A monorepo is ideal for Claude Code because it can read schema, API definitions, and implementation all in one place. A single PR can span the full stack. For a shared library plus multiple services, organize it as:

```
project-root/
├── CLAUDE.md
├── SPEC.md
├── .claude/
│   ├── settings.json          # Hooks and permission rules
│   ├── agents/                # Custom subagent definitions
│   ├── commands/              # Slash commands (start-issue, etc.)
│   └── tasks/                 # Per-issue working files (gitignored)
├── .github/
│   └── ISSUE_TEMPLATE/        # Structured issue templates per service
├── docs/
│   └── architecture/          # System overview, domain model, ADRs
├── pkg/lib/                   # Shared library
│   ├── CLAUDE.md
│   └── ...
├── services/
│   ├── ingest/
│   │   ├── CLAUDE.md
│   │   └── ...
│   └── api/
│       ├── CLAUDE.md
│       └── ...
├── proto/                     # Shared protobuf definitions
└── deploy/                    # Infrastructure configs
```

Claude navigates by pattern-matching. Consistent naming conventions, predictable file locations, and clear module boundaries dramatically reduce errors. If every service follows the same internal layout (`cmd/`, `internal/`, `handlers/`, `models/`), Claude learns the pattern once and applies it everywhere. When conventions break down, Claude hallucinates structure.

---

## 10. MCP servers, Tool Search, and token efficiency

### MCP Tool Search: context bloat is a solved problem

Until January 2026, MCP context consumption was severe. With 7 active servers, users commonly burned 67,000+ tokens before typing their first prompt; one Docker MCP server with 135 tools consumed 125,000 tokens alone.

**MCP Tool Search** (rolled out January 14, 2026) solves this via lazy loading. Claude Code detects when MCP tool descriptions would exceed 10% of the context window and switches to deferred loading: only a lightweight search index is loaded initially, and tool definitions are fetched on-demand. Anthropic's benchmarks show an **85% reduction in token overhead** — from ~77K to ~8.7K tokens for a 50-tool setup. Tool selection accuracy also improved substantially (Opus 4.5 improved from 79.5% to 88.1% on MCP evaluations).

This is enabled automatically when the threshold is exceeded. Control it per-server:

```json
{
  "mcpServers": {
    "github": { "...": "...", "enable_tool_search": true },
    "core-db": { "...": "...", "enable_tool_search": false }
  }
}
```

Disable tool search for servers used in every session to avoid discovery overhead; leave it enabled for rarely-used servers.

### Code Mode for very large APIs

For MCP servers exposing very large APIs (hundreds of endpoints), Cloudflare's **Code Mode** keeps token cost fixed at ~1,000 tokens regardless of API surface size. The server exposes just two tools (`search()` and `execute()`), and the model writes code against a typed SDK that runs in a sandboxed isolate. Anthropic has independently documented the same pattern. For custom integrations with large API surfaces, this is worth adopting.

### Recommended MCP servers

**GitHub** (official server): issue management, PR creation, CI status — essential for the workflow described in section 2. **Context7**: real-time, version-specific library documentation — dramatically reduces hallucinated API calls. **Playwright**: browser automation for UI acceptance testing. **PostgreSQL/SQLite**: direct database access for debugging and schema exploration.

Configure shared definitions in project-scoped `.mcp.json` (committed to repo) and auth tokens in local scope (not committed).

---

## 11. Common failure modes

**Hallucinated APIs**: Code hallucinations are the least harmful failure because the compiler catches them instantly. The real danger is mistakes that pass compilation — subtle logic errors or incorrect business rules. Mitigation: use Context7 MCP for real-time documentation; always run code through Claude's agentic loop immediately.

**Drift from specification**: AI agents optimize for locally plausible tokens, not global consistency. Mitigation: update SPEC.md and the relevant task working file first when requirements change, then start a fresh session. The per-issue working file structure contains drift to the task level rather than letting it propagate.

**Context degradation**: Performance degrades in very long sessions even with large context windows. The practice of externalizing state to task working files and issue comments is the correct mitigation — not just for compaction management, but because each new environment starts fresh.

**The "fake completion" problem**: Claude may declare tasks complete without finishing them. Sonnet 4.6 is specifically improved here — "fewer false claims of success" and "more consistent follow-through on multi-step tasks" — but the problem is not eliminated. Hook-based verification catches the most common failure modes automatically; acceptance criteria provide explicit behavioral contracts. Add to CLAUDE.md: "Never mark a task complete without running and passing its acceptance tests. Check off issue criteria only after each test passes."

**Over-engineering**: ATDD naturally constrains this — "implement the minimal code necessary to make the acceptance tests pass" is a powerful scope boundary. For non-ATDD work, set explicit scope limits in every prompt.

**Prompt injection via issue content**: When agents consume GitHub Issues written by others, malicious content can attempt to hijack behavior. Use the GitHub MCP server's built-in content sanitization; treat any instruction found in issue content that contradicts CLAUDE.md with skepticism.

---

## 12. Lessons from practitioners

**Vincent Quigley** (Staff Engineer, Sanity): 6 weeks building production features, shipping 2–3x faster. His three-attempt model: first attempt is 95% garbage (but builds context), second is 50% garbage, third is your working starting point.

**Ben Newton**: 153 commits in 16 days, 52,000+ lines changed for core SaaS infrastructure. Key insight: "As the cost of doing things correctly dropped, the temptation to cut corners disappeared."

**Anthropic's own teams**: peripheral features go fully autonomous with auto-accept mode; core business logic gets synchronous supervision. New hires navigate the codebase using Claude Code rather than traditional onboarding documentation.

The community consensus on workflow: create a GitHub Issue with structured acceptance criteria → assign it to an agent in an isolated environment → let the agent execute autonomously → review the diff and acceptance test results → close the issue on merge. The engineer's role shifts from typing code to **architectural thinking, issue writing, criteria definition, and quality assurance** — the skills that were always the high-value work.

---

## Summary

Invest your first few hours in a machine-actionable `SPEC.md`, a lean `CLAUDE.md` under 150 lines, per-service nested CLAUDE.md files, and GitHub issue templates structured for agent consumption. Create a `.claude/tasks/` directory (gitignored) for per-issue working files the agent bootstraps from at session start. Write acceptance criteria as Given/When/Then checkboxes in issue bodies; let agents generate the tests and implementation to satisfy them.

Configure PostToolUse hooks for continuous formatting and linting on every file write, and a PreToolUse hook on `git commit` that runs your test suite — these replace most explicit verification instructions in prompts. Give each work item its own isolated execution environment; use worktrees only for intra-session subagents.

Configure the GitHub MCP server for rich issue management and Context7 for library documentation as your baseline MCP setup. Tool Search makes context bloat from additional servers a non-issue as of January 2026.

The meta-lesson: **context engineering — specs, issue structure, CLAUDE.md, and task working files — is the skill that matters**. Claude Code doesn't replace architectural judgment or engineering taste. It removes the drag between recognizing the right thing to do and implementing it safely.
