-- name: CreateApp :one
INSERT INTO apps (slug) VALUES (@slug)
RETURNING id, slug, status, created_at;

-- name: GetAppIDBySlug :one
SELECT id FROM apps WHERE slug = @slug AND deleted_at IS NULL;

-- name: LockAppBySlug :one
-- Serialises deploy creation for one app inside the creating transaction.
SELECT id FROM apps WHERE slug = @slug AND deleted_at IS NULL FOR UPDATE;

-- name: HasUnfinishedDeploy :one
SELECT EXISTS (
    SELECT 1 FROM deploys
    WHERE app_id = @app_id AND state IN ('queued', 'building', 'built', 'starting', 'health_check')
);

-- name: CreateDeploy :one
-- seq is the app's next per-app counter; callers hold the app row lock.
INSERT INTO deploys (app_id, seq, source_ref)
VALUES (
    sqlc.arg(app_id)::uuid,
    (SELECT coalesce(max(d.seq), 0) + 1 FROM deploys d WHERE d.app_id = sqlc.arg(app_id)::uuid),
    @source_ref
)
RETURNING id, seq, state;
