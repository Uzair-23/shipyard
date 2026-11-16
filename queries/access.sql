-- name: CanViewApp :one
-- Owner, member (matched by email) or public. Used by the API and the router.
SELECT EXISTS (
    SELECT 1 FROM apps a
    WHERE a.id = @app_id AND a.deleted_at IS NULL
      AND (a.visibility = 'public'
           OR a.owner_id = @user_id
           OR EXISTS (SELECT 1 FROM app_members m WHERE m.app_id = a.id AND m.email = @email))
);

-- name: GetAppBySlug :one
SELECT id, slug, visibility FROM apps WHERE slug = @slug AND deleted_at IS NULL;

-- name: SetVisibility :exec
UPDATE apps SET visibility = @visibility WHERE id = @id;

-- name: ListMembers :many
SELECT email, created_at FROM app_members WHERE app_id = @app_id ORDER BY created_at, email;

-- name: CountMembers :one
SELECT count(*) FROM app_members WHERE app_id = @app_id;

-- name: AddMember :execrows
INSERT INTO app_members (app_id, email) VALUES (@app_id, @email) ON CONFLICT DO NOTHING;

-- name: RemoveMember :execrows
DELETE FROM app_members WHERE app_id = @app_id AND email = @email;

-- name: CreateHandoffCode :exec
INSERT INTO handoff_codes (code_hash, app_id, user_id, return_path, expires_at)
VALUES (@code_hash, @app_id, @user_id, @return_path, @expires_at);

-- name: ConsumeHandoffCode :one
-- Atomic and single-use: only an unused, unexpired code issued for this very app,
-- for a user who is not suspended.
UPDATE handoff_codes h SET used_at = now()
FROM users u
WHERE h.code_hash = @code_hash AND h.app_id = @app_id
  AND h.used_at IS NULL AND h.expires_at > now()
  AND u.id = h.user_id AND u.suspended_at IS NULL
RETURNING h.user_id, u.email, h.return_path;
