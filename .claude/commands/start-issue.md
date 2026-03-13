Load the GitHub issue number provided as `$ISSUE_NUMBER`.

Run: `gh issue view $ISSUE_NUMBER --json title,body,labels,comments`
Read `.claude/tasks/$ISSUE_NUMBER.md` if it exists.

---

## Step 0: Size assessment

Assess whether the issue is large before doing anything else. It is large if any of the following are true:
- Would touch more than ~10 files
- Spans multiple services or modules
- Contains multiple independent acceptance criteria that don't share a code path
- Has significant ambiguity — missing technical context, unclear boundaries, or implementation approach not evident from the issue

**If large → decomposition path:**
1. If the scope is ambiguous, ask the user one focused clarifying question and wait for an answer before continuing.
2. Determine how to split the issue into sub-issues. Each sub-issue must be atomic (one concern, one service/module, ≤~10 files).
3. For each sub-issue, write a high-level plan: what it does, its acceptance criteria (Given/When/Then), and any dependencies on other sub-issues.
4. Create the sub-issues: `gh issue create -t "<type>(<scope>): <title>" -b "<body>"` using the standard template. Link each to the parent in its Technical Context section.
5. Post a decomposition summary as a comment on the parent issue listing the sub-issues created and their dependency order.
6. Label the parent: `gh issue edit $ISSUE_NUMBER --add-label "status:decomposed"`
7. **Stop.** Present the decomposition to the user for review. Do not implement.

**If small → proceed with steps 1–6 below.**

---

## Steps 1–6: Implementation

1. **Self-assign**: `gh issue edit $ISSUE_NUMBER --add-label "status:in-progress" --remove-label "status:todo"`
2. **Create branch**: `gh issue develop $ISSUE_NUMBER -c --base main`
   Naming convention: `feat/<scope>-<issue-number>` or `fix/<scope>-<issue-number>`.
3. **Post plan**: Post a brief implementation plan as an issue comment before writing any code.
4. **Implement**: Work through acceptance criteria checkboxes. Check each off in the issue body as it is completed.
5. **Open PR**: `gh pr create -t "<type>(<scope>): <title>" -b "Fixes #$ISSUE_NUMBER"`
