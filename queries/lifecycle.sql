-- name: GetAppForLifecycle :one
SELECT id, desired_state, status, current_deploy_id, deleted_at FROM apps WHERE id = @id;

-- name: ActiveInstancesOfApp :many
-- Every instance of the app that is, or is becoming, a running container.
SELECT i.id FROM instances i JOIN deploys d ON d.id = i.deploy_id
WHERE d.app_id = sqlc.arg(app_id)::uuid AND i.state IN ('starting', 'running');

-- name: SetAppStatus :exec
UPDATE apps SET status = @status::text WHERE id = @id;
