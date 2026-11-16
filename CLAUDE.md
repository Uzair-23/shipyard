# CLAUDE.md

Instructions for Claude Code working on **Shipyard**. Part A is project context. Part B is behavioural guidelines. Both apply to every session.

**Tradeoff:** These guidelines bias toward caution over speed. For trivial tasks, use judgment.

**Current mode: 3-day finish plan** (`docs/ROADMAP.md`, ADR 014). Prefer the simplest change that meets the current block's "Done when". Reuse the M0/M1 code; don't rewrite working code.

---

# Part A: Project

## What Shipyard is
A self-hostable platform that turns a folder of code into a private, shareable web app on its own subdomain, with login, secrets, a database, logs and scale-to-zero. Think "Vercel for internal tools." It runs on **one machine** with `docker compose up`, and it must use **zero paid resources**.

## Project docs (private, gitignored, read them)
@docs/PROGRESS.md

Read these when the task touches their area. Don't load all of them every time:
- `docs/ROADMAP.md`: the 3-day plan. **Always work on the next unticked block.** Its "Shared contracts" section (process topology, migrations, API table) is binding.
- `docs/decisions/014-three-day-finish-plan.md`: scope and simplifications. **ROADMAP and ADR 014 override PRD and ARCHITECTURE when they disagree.**
- `docs/PRD.md`: requirements (IDs like `APP-2`, `RUN-1`), quotas, performance targets.
- `docs/ARCHITECTURE.md`: components, data model, request flows, container spec, security model.
- `docs/decisions/NNN-*.md`: accepted decisions (ADRs).

## Session protocol
**At the start:**
1. Read `docs/PROGRESS.md` (imported above): current block, next step, open issues.
2. Read that block in `docs/ROADMAP.md`. Read an ARCHITECTURE section only if the block needs it.
3. Enter plan mode once: state the plan with a verification check per step (see §4 in Part B), then build without further check-ins unless something is unclear.

**At the end of the session (once, not after every task):**
1. Update `docs/PROGRESS.md`: what was done, tests, commit hashes, problems, next step.
2. Tick finished boxes in `docs/ROADMAP.md`.
3. If you stopped mid-feature, record the exact state under "Open issues". Do not commit unfinished work.

**Pace:** if a block is taking much longer than its estimate, stop and say so, and propose cuts from ROADMAP "If behind schedule".

## Decisions are binding
- Never contradict an **Accepted** ADR in `docs/decisions/`. ADR 014 amends some of them for this phase; follow 014 where they differ. If one seems wrong, stop, explain why, and propose a new ADR. Write it only after the human agrees.
- During the 3-day plan, only these need an ADR: new services, new external dependencies, or anything that contradicts ROADMAP or ADR 014. Smaller choices go in the PROGRESS.md log.
- Don't add new services or infrastructure (Redis, Kafka, Kubernetes, paid APIs, SaaS) without an ADR. Postgres is the queue, pub/sub and log store (ADR 002).

## Stack
- **Backend:** Go (version per `go.mod`), one binary `cmd/shipyard` with roles `api`, `worker`, `router`, `migrate` (ADR 003). For this phase the API runs in compose; the worker and router run on the host (ADR 014). Cross-process signals go through Postgres (`NOTIFY route_changes`, jobs, `handoff_codes`). Libraries: `chi`, `pgx`, `sqlc`, `goose`, `log/slog`, Docker Go SDK, `cobra` (CLI).
- **Database:** PostgreSQL 16 (platform DB) plus a separate `apps-db` instance for user databases (ADR 008).
- **Containers:** Docker Engine; gVisor `runsc` when available, else `runc` (ADR 007).
- **Edge:** Caddy. Dev domains are `shipyard.localhost` and `*.apps.localhost` (ADR 010).
- **Frontend:** React + TypeScript + Vite + TanStack Query + Tailwind in `web/`.
- **Tests:** Go `testing`, `testcontainers-go`, Playwright, k6.

## Commands
Always use `make` targets. Don't invent ad-hoc commands when a target exists.

| Command | Purpose |
|---|---|
| `make dev` | Start the full local stack (compose + Vite) |
| `make build` | Build binaries and dashboard |
| `make test` | Unit tests |
| `make test-integration` | Tests against real Postgres/Docker |
| `make test-security` | Security suite (`security/`) |
| `make lint` / `make fmt` | Lint / format Go and TS |
| `make migrate` | Apply DB migrations |
| `make sqlc` | Regenerate query code after editing `queries/` |
| `make bench` | k6 benchmarks |

If a needed target doesn't exist yet, add it to the `Makefile` rather than documenting a raw command.

## Code layout (short version, full tree in ARCHITECTURE §16)
- `cmd/shipyard/` (server roles), `cmd/shipyard-cli/` (user CLI)
- `internal/<area>/`: `api`, `auth`, `apps`, `access`, `build`, `runtime`, `router`, `jobs`, `reconcile`, `secrets`, `appdb`, `store`, `metrics`, `config`
- `migrations/` (goose SQL), `queries/` (sqlc SQL), `web/` (dashboard), `security/`, `bench/`, `testdata/apps/`

**Boundary rules:**
- `internal/api` must **not** import `internal/runtime` or talk to Docker. The API writes desired state and enqueues jobs; the worker executes them.
- Access checks live only in `internal/access` and are shared by api and router. Don't duplicate permission logic.
- All SQL goes through `sqlc` queries in `queries/`. No string-built SQL.

## Security rules (non-negotiable; user code is untrusted)
Violating any of these is a bug, even if tests pass.
- App containers **never** get the Docker socket, host mounts, host networking or privileged mode. Host ports: only the existing `127.0.0.1` random-port publishing (ADR 014 amends ADR 009 for this phase), never `0.0.0.0`.
- Every app container uses the full spec in ARCHITECTURE §10: non-root UID 10001, `cap-drop ALL`, `no-new-privileges`, read-only rootfs + tmpfs `/tmp`, memory/CPU/PID limits.
- Untrusted users build **only from templates** in `internal/build/templates/`. No user Dockerfiles (ADR 004).
- The dashboard session cookie is host-only on `shipyard.DOMAIN`. The router **strips** `_sy_app` and every incoming `X-Shipyard-*` header before proxying (ADR 005).
- Every app API endpoint checks ownership/role. Default is deny.
- Secrets: AES-256-GCM via `internal/secrets`, never logged, never returned by the API after creation.
- Uploads: reject absolute paths, `..`, outside-pointing symlinks, and anything over the size/file limits.
- Passwords use argon2id; tokens are stored as SHA-256 hashes; use constant-time comparisons.
- Never read or print `.env`. Use `.env.example` for variable names.
- When you change anything in `runtime/`, `router/`, `auth/`, `access/`, `build/` or `secrets/`, re-check the list above yourself and say so in your summary.
- **3-day plan (ADR 014):** don't run the `security-reviewer` agent, skip that step in `/ship-check` if the skill includes it, and don't write new `security/` tests. Existing security tests must keep passing.

## Coding conventions
**Go**
- Errors: wrap with context (`fmt.Errorf("start container %s: %w", id, err)`). No `panic` outside `main`.
- Pass `context.Context` as the first argument to anything that does I/O; respect cancellation and timeouts.
- Logging: `log/slog` structured fields (`app_id`, `job_id`, `request_id`). Never log secrets or tokens.
- Job handlers must be **idempotent** and safe to retry (ADR 002).
- Keep interfaces small and defined where they're used. Don't create an interface until there are two implementations or a test needs it.

**TypeScript / React**
- Strict TS, no `any`. Server state via TanStack Query; no global state library unless an ADR adds one.
- API types are hand-written in `web/src/types.ts` and must match the ROADMAP API table. All calls go through `web/src/api.ts`.
- The dashboard is limited to the 4 pages in ROADMAP Block 2B.

**Database**
- Every schema change is a new goose migration. Never edit an applied migration.
- Add indexes for every foreign key and every column used in a `WHERE` on a hot path.

## Testing
- New behaviour needs tests. Bug fixes start with a failing test that reproduces the bug.
- Integration tests use real Postgres/Docker via testcontainers. Don't mock the database.
- Keep tests lean: unit tests for tricky logic (cookie signing and stripping, handoff codes, secrets, wake singleflight, ownership) plus a few integration tests. No new `security/` tests this phase.
- Sample apps for tests live in `testdata/apps/`. Reuse them before creating new ones.

## Git workflow: commit and push each working feature
**Commit when a feature works.** A feature is one or a few related checkboxes in the current block. All of these must be true:
1. The feature is implemented, with no TODOs left for its core behaviour.
2. `make lint` and `make test` pass. If the change touches DB, Docker, router or build, `make test-integration` passes too.
3. `make test-security` still passes if you touched `build/`, `runtime/` or the upload endpoint.
4. The feature was exercised once for real (deploy a sample app, curl the endpoint, open the page), not just unit-tested.

If any check fails, **do not commit**. Fix it, or record the state in `docs/PROGRESS.md` → "Open issues" and stop.

**Then, in this order:**
1. `git status`. Confirm that nothing under `docs/`, no `.env*` (except `.env.example`), no keys, no `CLAUDE.local.md`, and no build output is staged. Unstage with `git restore --staged <path>` if needed.
2. Stage only this task's files.
3. Commit with a Conventional Commits message, **one commit per working feature** (at least one per block):
   ```
   <type>(<scope>): <imperative summary, ≤ 72 chars>

   <what changed and why, 1–5 lines>

   Roadmap: <block id, e.g. 1A> - <feature>
   ```
   Types: `feat`, `fix`, `refactor`, `test`, `docs`, `chore`, `perf`, `ci`, `build`, `security`. Scope = package or area (`router`, `build`, `jobs`, `web`, `cli`, …).
4. Push immediately: `git push origin <current branch>`.
   - Rejected because the remote moved → `git pull --rebase`, re-run `make test`, push again.
   - Day 2 worktrees commit to `day2-backend` and `day2-web`. Merge into `main` only after both work together and `make test` passes, then push `main`.
   - Auth or network failure → stop and tell the human. Leave the commit local.
5. Note the short hash (`git rev-parse --short HEAD`) for the end-of-session PROGRESS.md entry.
6. If the `gh` CLI is available, check CI with `gh run list --limit 1`. If CI is red, fixing it is the next task.

**Never:** force-push (`--force`, `-f`, `--force-with-lease`), `git reset --hard`, rewrite pushed history, commit `docs/` or `.env`, commit failing tests, or bundle unrelated features into one commit.

## Never do
- Add paid services, API keys for paid tools, or anything that breaks the zero-cost constraint.
- Run `sudo`, `rm -rf`, `docker system prune`, or `docker volume rm` without the human asking.
- Change API shapes without updating the ROADMAP API table and `web/src/api.ts` (and `api/openapi.yaml` when practical).
- Start a later block before the current block's "Done when" checks pass.
- Build anything from ROADMAP "Later" without the human asking.

---

# Part B: Behavioural guidelines

## 1. Think Before Coding

**Don't assume. Don't hide confusion. Surface tradeoffs.**

Before implementing:
- State your assumptions explicitly. If uncertain, ask.
- If multiple interpretations exist, present them. Don't pick silently.
- If a simpler approach exists, say so. Push back when warranted.
- If something is unclear, stop. Name what's confusing. Ask.
- If the task conflicts with the PRD, ARCHITECTURE or an ADR, say so before coding.

## 2. Simplicity First

**Minimum code that solves the problem. Nothing speculative.**

- No features beyond what was asked. A later block's or "Later" list's feature is not "asked" yet.
- No abstractions for single-use code.
- No "flexibility" or "configurability" that wasn't requested.
- No error handling for impossible scenarios.
- If you write 200 lines and it could be 50, rewrite it.

Ask yourself: "Would a senior engineer say this is overcomplicated?" If yes, simplify.

Exception: **security hardening is never "speculative."** The Security rules in Part A always apply.

## 3. Surgical Changes

**Touch only what you must. Clean up only your own mess.**

When editing existing code:
- Don't "improve" adjacent code, comments, or formatting.
- Don't refactor things that aren't broken.
- Match existing style, even if you'd do it differently.
- If you notice unrelated dead code or bugs, mention them and add them to "Open issues" in `docs/PROGRESS.md`. Don't fix them silently.

When your changes create orphans:
- Remove imports/variables/functions that YOUR changes made unused.
- Don't remove pre-existing dead code unless asked.

The test: every changed line should trace directly to the current ROADMAP block.

## 4. Goal-Driven Execution

**Define success criteria. Loop until verified.**

Transform tasks into verifiable goals:
- "Add validation" → "Write tests for invalid inputs, then make them pass"
- "Fix the bug" → "Write a test that reproduces it, then make it pass"
- "Refactor X" → "Ensure tests pass before and after"
- "Implement a ROADMAP block" → "Meet that block's 'Done when' checks"

For multi-step tasks, state a brief plan:
```
1. [Step] → verify: [check]
2. [Step] → verify: [check]
3. [Step] → verify: [check]
```

Strong success criteria let you loop independently. Weak criteria ("make it work") require constant clarification.

When a task affects a PRD §7.1 performance target (deploy time, cold start, proxy overhead, throughput), measure it and record the number in the `docs/PROGRESS.md` Benchmarks table.

---

**These guidelines are working if:** fewer unnecessary changes in diffs, fewer rewrites due to overcomplication, clarifying questions come before implementation rather than after mistakes, and every pushed commit leaves `main` green.