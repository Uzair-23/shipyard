-- name: MarkDeployStarting :execrows
UPDATE deploys SET state = 'starting'
WHERE id = @id AND state IN ('built', 'starting');

-- name: GetActiveInstance :one
-- The instance a start job is (re)starting; reused on retry so the job is idempotent.
SELECT * FROM instances
WHERE deploy_id = @deploy_id AND state IN ('starting', 'running')
ORDER BY created_at DESC
LIMIT 1;

-- name: CreateInstance :one
INSERT INTO instances (deploy_id) VALUES (@deploy_id)
RETURNING *;

-- name: SetInstanceContainer :exec
UPDATE instances
SET container_id = @container_id::text, host_port = @host_port::int, started_at = coalesce(started_at, now())
WHERE id = @id;

-- name: FailInstance :exec
UPDATE instances SET state = 'failed', stopped_at = now()
WHERE id = @id AND state IN ('starting', 'running');

-- name: SetAppStatus :exec
UPDATE apps SET status = @status::text WHERE id = @id;
