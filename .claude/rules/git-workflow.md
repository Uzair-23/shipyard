# Git workflow

Full procedure is in CLAUDE.md ("Git workflow"). Summary:

- Commit only when a ROADMAP task is complete and working: implemented, `make lint test` green,
  `make test-integration` green when DB/Docker/router/build is touched, security list re-checked,
  and the feature exercised once for real.
- One commit per ROADMAP task, Conventional Commits: `<type>(<scope>): <summary ≤ 72 chars>`,
  short body, and a `Roadmap: M<n> - <task name>` trailer.
- Before committing: `git status`; nothing under `docs/`, no `.env*` (except `.env.example`), no keys,
  no `CLAUDE.local.md`, no build output. Stage only this task's files.
- Push with `git push origin main`. If rejected: `git pull --rebase origin main`, re-run `make test`, push again.
  On auth/network failure stop and tell the human; leave the commit local.
- Afterwards record the short hash in `docs/PROGRESS.md`, tick the task in `docs/ROADMAP.md`,
  and check CI with `gh run list --limit 1` if `gh` is available.
- Never: force-push, `git reset --hard`, rewrite pushed history, commit `docs/` or `.env`,
  commit failing tests, bundle several tasks in one commit.
- `.claude/hooks/guard-git.sh` enforces the destructive-command and private-file rules.
