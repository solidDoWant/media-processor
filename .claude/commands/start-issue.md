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

**If small → proceed with steps 1–5 below.**

---

## Steps 1–5: Implementation

1. **Self-assign**: `gh issue edit $ISSUE_NUMBER --add-label "status:in-progress"`. Also remove `status:todo` if it exists.
2. **Post plan**: Use planning mode to draft a brief implementation plan covering approach, files to change, and how each acceptance criterion will be satisfied. Post it as an issue comment. Write the plan and current state to `.claude/tasks/$ISSUE_NUMBER.md`. Then **stop and ask the user (in the chat) to review the plan and approve it before you proceed**. Do not write any code until the user explicitly approves. On re-invocation, if a plan is already recorded in `.claude/tasks/$ISSUE_NUMBER.md` and no approval is noted, re-present the plan and ask again.
3. **Create branch**: `gh issue develop $ISSUE_NUMBER -c --base main`
   Naming convention: `feat/<scope>-<issue-number>` or `fix/<scope>-<issue-number>`.
4. **Implement**: Follow the approved plan from `.claude/tasks/$ISSUE_NUMBER.md` (and the corresponding issue comment) step by step. Do not deviate without checking with the user first. Work through acceptance criteria checkboxes, checking each off in the issue body as it passes.
5. **Open PR**: `gh pr create -t "<type>(<scope>): <title>" -b "Fixes #$ISSUE_NUMBER"`
