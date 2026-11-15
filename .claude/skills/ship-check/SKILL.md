---
name: ship-check
description: Run when a ROADMAP task is finished - verifies, commits (one commit per task) and pushes per CLAUDE.md.
---
1. Run `make lint` and `make test`; also `make test-integration` if DB, Docker, router or build code changed. Fix failures.
2. If files under `internal/runtime/`, `internal/router/`, `internal/auth/`, `internal/access/`, `internal/build/` or `internal/secrets/` changed, run the security-reviewer agent and fix findings; run `make test-security` once it exists.
3. Confirm the feature was exercised for real (curl, deploy a sample app, open the page).
4. Follow CLAUDE.md "Git workflow": check `git status` for private files, stage only this task's files, commit with a Conventional Commits message (`Roadmap: M<n> - <task>` trailer), then `git push origin main`. Never force-push.
5. Update `docs/PROGRESS.md` (what was done, tests, commit hash, problems, next) and tick the task in `docs/ROADMAP.md`.
6. If `gh` is available, check CI with `gh run list --limit 1`.
If any check fails, do not commit; record the state under "Open issues" in `docs/PROGRESS.md`.
