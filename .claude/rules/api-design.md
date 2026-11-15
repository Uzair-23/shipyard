# API design rules

These are proposed defaults for the REST API; adjust before M3 if you disagree.

- All routes live under `/api/v1`; plain JSON in and out; `Content-Type: application/json`
  (multipart only for source uploads).
- `api/openapi.yaml` is the source of truth. Change an endpoint → update the spec in the same commit.
  Dashboard TypeScript types come from the spec; don't hand-write duplicates.
- Resources are plural nouns: `/apps`, `/apps/{slug}`, `/apps/{slug}/deploys/{seq}`. Actions that
  aren't CRUD use a verb subpath: `POST /apps/{slug}/restart`.
- Status codes: 200/201 success, 204 no body, 400 invalid input, 401 unauthenticated, 403 forbidden,
  404 not found (also for resources the caller may not know exist), 409 conflict, 429 rate limited.
  A user requesting another user's app gets 404, never 403.
- Error body: `{"error": {"code": "<machine_code>", "message": "<what happened and what to do next>"}}`.
- Lists are paginated with `limit` + opaque `cursor`; responses return `next_cursor` when more exist.
- Secrets are write-only: responses contain names, never values.
- Authentication: session cookie (dashboard, with CSRF header on mutations) or `Authorization: Bearer shp_…`.
- The API never talks to Docker; it records desired state and enqueues jobs, then returns.
- Never put secrets, tokens or passwords in logs, URLs or error messages.
