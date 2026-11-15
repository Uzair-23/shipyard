# Security rules

User code is untrusted. Violating any rule below is a bug even if tests pass.
Sources: CLAUDE.md "Security rules", ARCHITECTURE §7, §10, §11, ADRs 004, 005, 009.

## Containers
- Never give an app container the Docker socket, host mounts, host networking, published ports or privileged mode.
- Every app container: UID 10001, `cap-drop ALL`, `no-new-privileges`, read-only rootfs + tmpfs `/tmp`,
  memory/CPU/PID/nofile limits, own bridge network `sy-app-<id>`, restart policy `no`.
- `runsc` (gVisor) when available, else `runc` with a logged warning.

## Builds and uploads
- Untrusted users build only from templates in `internal/build/templates/`. No user Dockerfiles.
- Reject absolute paths, `..`, symlinks pointing outside the root, > 50 MB, > 5,000 files.

## Auth and access
- Dashboard cookie is host-only (`__Host-` prefix) on `shipyard.DOMAIN`; apps never receive it.
- Router strips `_sy_app` and every incoming `X-Shipyard-*` header, then sets trusted identity headers.
- `_sy_app` is HMAC-signed and bound to `app_id`; a cookie for app A must fail on app B.
- Every app endpoint checks ownership/role; default is deny. Access logic lives only in `internal/access`.
- Passwords: argon2id. Tokens: stored as SHA-256 hashes. Compare secrets in constant time.

## Secrets and data
- AES-256-GCM via `internal/secrets`, associated data `app_id|key`. Never logged, never returned after creation.
- All SQL via sqlc queries; no string-built SQL.
- Never read or print `.env`; use `.env.example` for variable names.

## Process
- Changes in `runtime/`, `router/`, `auth/`, `access/`, `build/`, `secrets/` get the `security-reviewer` agent
  and a test in `security/` proving the attack fails.
