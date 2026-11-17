-- name: CreateApp :one
INSERT INTO apps (slug, owner_id) VALUES (@slug, @owner_id)
RETURNING id;

-- name: GetOwnedAppID :one
-- Every app route looks the app up through its owner: someone else's app is "not found".
SELECT id FROM apps WHERE slug = @slug AND owner_id = @owner_id AND deleted_at IS NULL;

-- name: LockOwnedApp :one
-- Serialises changes to one app inside the changing transaction.
SELECT id, current_deploy_id, desired_state FROM apps
WHERE slug = @slug AND owner_id = @owner_id AND deleted_at IS NULL
FOR UPDATE;

-- name: ListOwnedApps :many
SELECT slug, status, created_at FROM apps
WHERE owner_id = @owner_id AND deleted_at IS NULL
ORDER BY created_at DESC
LIMIT 200;

-- name: GetOwnedApp :one
SELECT a.id, a.slug, a.status, a.desired_state, a.visibility, a.idle_timeout_s, a.last_cold_start_ms, a.created_at,
       cd.seq AS current_seq, cd.state AS current_state, cd.finished_at AS current_finished_at,
       coalesce(ld.seq, 0)::int AS latest_seq, coalesce(ld.state, '')::text AS latest_state, ld.error AS latest_error,
       ld.created_at AS latest_created_at, ld.finished_at AS latest_finished_at
FROM apps a
LEFT JOIN deploys cd ON cd.id = a.current_deploy_id
LEFT JOIN LATERAL (
    SELECT seq, state, error, created_at, finished_at FROM deploys
    WHERE app_id = a.id ORDER BY seq DESC LIMIT 1
) ld ON true
WHERE a.slug = @slug AND a.owner_id = @owner_id AND a.deleted_at IS NULL;

-- name: SetIdleTimeout :exec
UPDATE apps SET idle_timeout_s = @idle_timeout_s WHERE id = @id;

-- name: SetDesiredState :exec
UPDATE apps SET desired_state = @desired_state::text WHERE id = @id;

-- name: SoftDeleteApp :exec
-- The slug is released (unique only among non-deleted apps); a stop job removes the containers.
UPDATE apps SET deleted_at = now(), desired_state = 'stopped' WHERE id = @id;

-- name: FailUnfinishedDeploys :exec
-- Deleting an app: pending build/start jobs find their deploy failed and do nothing.
UPDATE deploys SET state = 'failed', error = 'the app was deleted', finished_at = now()
WHERE app_id = @app_id AND state IN ('queued', 'building', 'built', 'starting', 'health_check');

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
