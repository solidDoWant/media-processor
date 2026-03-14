Load the GitHub issue number provided as `$ISSUE_NUMBER`.

Run: `gh issue view $ISSUE_NUMBER --json title,body,labels,comments`
Read `.claude/tasks/$ISSUE_NUMBER.md` if it exists.

---

## Issue engagement (ongoing throughout all steps)

- **Answer questions**: Read all comments on the issue. If any commenter has asked a question that hasn't been answered, post a reply answering it before proceeding.
- **Ask questions**: If at any point you need information you cannot determine from the issue, codebase, or existing comments, post a question as an issue comment and ask the user to reply on the issue. Then stop and wait — do not proceed until you have an answer.
- Before stopping to wait for a reply: write your current state (what you've done, what you're waiting on, any decisions made so far) to `.claude/tasks/$ISSUE_NUMBER.md`, then inform the user: "I've posted a question on issue #$ISSUE_NUMBER. Please reply there and then re-run this command." When re-invoked, read that file to resume where you left off.

---

## Step 0: Size assessment

Assess whether the issue is large before doing anything else. It is large if any of the following are true:
- Would touch more than ~10 files
- Spans multiple services or modules
- Contains multiple independent acceptance criteria that don't share a code path
- Has significant ambiguity — missing technical context, unclear boundaries, or implementation approach not evident from the issue

**If large → decomposition path:**
1. If the scope is ambiguous, save current state to `.claude/tasks/$ISSUE_NUMBER.md`, post a focused clarifying question as an issue comment, and stop. Do not continue until answered.
2. Determine how to split the issue into sub-issues. Each sub-issue must be atomic (one concern, one service/module, ≤~10 files).
3. For each sub-issue, write a high-level plan: what it does, its acceptance criteria (Given/When/Then), and any dependencies on other sub-issues.
4. Create the sub-issues: `gh issue create -t "<type>(<scope>): <title>" -b "<body>"` using the standard template. Link each to the parent in its Technical Context section.
5. Post a decomposition summary as a comment on the parent issue listing the sub-issues created and their dependency order.
6. Label the parent: `gh issue edit $ISSUE_NUMBER --add-label "status:decomposed"`
7. **Stop.** Present the decomposition to the user for review. Do not implement.

**If small → proceed with steps 1–6 below.**

---

## Steps 1–6: Implementation

1. **Self-assign**: `gh issue edit $ISSUE_NUMBER --add-label "status:in-progress"`. Also remove `status:todo` if it exists.

2. **Post plan**: Draft a brief written implementation plan covering: approach, files to change, and how each acceptance criterion will be satisfied. Do not use plan mode or take any action yet — just think and write. Then follow this approval loop:

   **If no plan exists yet in the task file:**
   - Post the plan as an issue comment.
   - Write the plan to `.claude/tasks/$ISSUE_NUMBER.md` with `status: pending-approval`.
   - Stop and ask the user (in the chat) to review the plan. Do not write any code until approved.

   **If a plan exists with `status: pending-approval`:**
   - Read all issue comments posted after the plan comment.
   - **Check for explicit approval**: a comment counts as approval only if it clearly and unconditionally says to proceed — e.g. "approved", "lgtm", "looks good, proceed", "go ahead". When in doubt, treat it as feedback, not approval.
   - If the latest relevant comment is explicit approval (and no subsequent comment adds new feedback): update the task file to `status: approved` and proceed to Step 3.
   - Otherwise (comment contains questions, change requests, or ambiguous language — or there are no new comments): treat as feedback. Revise the plan to address each piece of feedback, post the revised plan as a **new** issue comment, overwrite the plan in the task file (keeping `status: pending-approval`), and stop — ask the user to review the updated plan.

   **If the task file has `status: approved`:** skip directly to Step 3.

3. **Create branch**: Derive the branch name from the issue's conventional commit prefix and number: `feat/<scope>-<issue-number>` or `fix/<scope>-<issue-number>` (e.g. `fix/start-issue-12`). The scope is the parenthetical from the issue title (e.g. `fix(start-issue): ...` → scope is `start-issue`). Then run:
   ```
   gh issue develop $ISSUE_NUMBER -c --base master --name <branch-name>
   ```
   After creating the branch, record the branch name in `.claude/tasks/$ISSUE_NUMBER.md`.

4. **Implement**: Follow the approved plan from `.claude/tasks/$ISSUE_NUMBER.md` (and the corresponding issue comment) step by step. Do not deviate without checking with the user first. Work through acceptance criteria checkboxes, checking each off in the issue body as it passes.

5. **Open PR**: `gh pr create -t "<type>(<scope>): <title>" -b "Fixes #$ISSUE_NUMBER"`
   After creating the PR, record the PR number in `.claude/tasks/$ISSUE_NUMBER.md`.

6. **PR feedback loop**: On re-invocation after a PR is open, check for reviewer feedback:
   - Look up the PR using the PR number stored in the task file, or via `gh pr list --head <branch> --json number,state`.
   - Fetch review comments and threads: `gh pr view <PR_NUMBER> --json reviews,comments,reviewThreads`.
   - For each unresolved review thread or comment that has not yet been replied to by this agent:
     1. Make the necessary code change(s) to address the comment.
     2. Commit the change with a descriptive message.
     3. Reply to the specific thread on the PR using `gh api repos/{owner}/{repo}/pulls/{PR_NUMBER}/comments` with `in_reply_to` set to the comment ID, briefly describing what was changed.
   - After addressing all comments, push the updated branch.
   - If all threads are resolved or there are no unaddressed comments, report status to the user and stop.
   - **On merge**: delete the task file: `rm .claude/tasks/$ISSUE_NUMBER.md`.
