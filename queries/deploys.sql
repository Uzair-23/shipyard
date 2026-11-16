-- name: GetDeployForBuild :one
SELECT d.id, d.app_id, d.seq, d.source_ref, d.state, d.image_digest, d.config_snapshot
FROM deploys d
WHERE d.id = @id;

-- name: MarkDeployBuilding :execrows
UPDATE deploys SET state = 'building'
WHERE id = @id AND state IN ('queued', 'building');

-- name: MarkDeployBuilt :execrows
UPDATE deploys SET state = 'built', image_digest = @image_digest::text, config_snapshot = @config_snapshot
WHERE id = @id AND state = 'building';

-- name: FailDeploy :execrows
-- Records a user-visible failure; live or superseded deploys are never failed.
UPDATE deploys SET state = 'failed', error = @error::text, finished_at = now()
WHERE id = @id AND state NOT IN ('live', 'superseded', 'failed');
