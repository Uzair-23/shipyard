---
name: plan-milestone
description: Plan the next unticked ROADMAP task before coding - states the task, plan and success checks. Use at the start of a session or before starting a new task.
---
1. Read `docs/PROGRESS.md` (current milestone, next task, open issues).
2. Find the first unticked task in the current milestone of `docs/ROADMAP.md`; read its "Done when" checks.
3. Read the relevant `docs/ARCHITECTURE.md` section, PRD requirement IDs and any ADR that applies.
4. Output, and wait for the human's go-ahead if anything is ambiguous or conflicts with an ADR:
   - Task and the milestone "Done when" checks it contributes to.
   - Assumptions and open questions.
   - Numbered plan, each step with a `verify:` check.
   - Files to be created or changed, and whether the security-reviewer agent is needed.
5. Do not write code in this skill.
