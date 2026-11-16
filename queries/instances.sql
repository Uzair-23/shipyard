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


-- name: MarkDeployHealthCheck :execrows
UPDATE deploys SET state = 'health_check'
WHERE id = @id AND state IN ('starting', 'health_check');

-- name: LockAppForPromotion :exec
-- Serialises promotions of one app inside the promoting transaction.
SELECT id FROM apps WHERE id = @id FOR UPDATE;

-- name: MarkDeployLive :execrows
-- Newest wins: a deploy never goes live over a newer live deploy of the app.
UPDATE deploys SET state = 'live', finished_at = now()
WHERE deploys.id = sqlc.arg(id)::uuid AND deploys.state = 'health_check'
  AND NOT EXISTS (
      SELECT 1 FROM deploys newer
      WHERE newer.app_id = deploys.app_id AND newer.state = 'live' AND newer.seq > deploys.seq);

-- name: MarkDeploySupersededIfPending :execrows
-- An older deploy that lost the race to a newer live one.
UPDATE deploys SET state = 'superseded', finished_at = now()
WHERE id = @id AND state IN ('starting', 'health_check');

-- name: SupersedeOtherLiveDeploys :exec
-- Must run before MarkDeployLive (one live deploy per app, unique index).
-- Only older deploys are superseded; MarkDeployLive refuses to go live over a newer one.
UPDATE deploys SET state = 'superseded'
WHERE deploys.app_id = sqlc.arg(app_id)::uuid AND deploys.state = 'live' AND deploys.id <> sqlc.arg(keep_id)::uuid
  AND deploys.seq < (SELECT k.seq FROM deploys k WHERE k.id = sqlc.arg(keep_id)::uuid);

-- name: ActiveInstancesOfSupersededDeploys :many
-- Instances that must be stopped because their deploy was superseded.
SELECT i.id FROM instances i JOIN deploys d ON d.id = i.deploy_id
WHERE d.app_id = sqlc.arg(app_id)::uuid AND d.state = 'superseded' AND i.state IN ('starting', 'running');

-- name: SetInstanceState :exec
UPDATE instances SET state = @state::text,
    stopped_at = CASE WHEN @state::text IN ('stopped', 'failed') THEN now() ELSE stopped_at END
WHERE id = @id;

-- name: SetAppLive :exec
UPDATE apps SET current_deploy_id = @deploy_id, status = 'running' WHERE id = @id;

-- name: SetAppStarting :exec
-- An app that already serves a live deploy keeps showing "running" during a redeploy.
UPDATE apps SET status = 'starting'
WHERE apps.id = sqlc.arg(app_id)::uuid
  AND NOT EXISTS (SELECT 1 FROM deploys d WHERE d.app_id = sqlc.arg(app_id)::uuid AND d.state = 'live');

-- name: RestoreAppStatus :exec
-- After a failed deploy: running if a live deploy remains, else stopped.
UPDATE apps SET status = CASE
    WHEN EXISTS (SELECT 1 FROM deploys d WHERE d.app_id = sqlc.arg(app_id)::uuid AND d.state = 'live') THEN 'running'
    ELSE 'stopped' END
WHERE apps.id = sqlc.arg(app_id)::uuid;
