-- name: GetAppForLifecycle :one
SELECT id, desired_state, status, current_deploy_id, deleted_at FROM apps WHERE id = @id;

-- name: ActiveInstancesOfApp :many
-- Every instance of the app that is, or is becoming, a running container.
SELECT i.id FROM instances i JOIN deploys d ON d.id = i.deploy_id
WHERE d.app_id = sqlc.arg(app_id)::uuid AND i.state IN ('starting', 'running');

-- name: SetAppStatus :exec
UPDATE apps SET status = @status::text WHERE id = @id;

-- name: ListIdleApps :many
-- Running apps that have served no request for longer than their idle timeout.
-- The newest running instance counts as activity, so an app that was just
-- (re)started is not put back to sleep because of an old last_request_at.
-- Apps that already have a queued or running sleep job are skipped.
SELECT a.id FROM apps a
WHERE a.deleted_at IS NULL AND a.status = 'running' AND a.desired_state = 'running'
  AND a.current_deploy_id IS NOT NULL
  AND greatest(
        coalesce(a.last_request_at, '-infinity'::timestamptz),
        coalesce((SELECT max(i.created_at) FROM instances i
                  WHERE i.deploy_id = a.current_deploy_id AND i.state = 'running'),
                 '-infinity'::timestamptz)
      ) < now() - a.idle_timeout_s * interval '1 second'
  AND NOT EXISTS (SELECT 1 FROM jobs j
                  WHERE j.app_id = a.id AND j.kind = 'sleep' AND j.state IN ('queued', 'running'));

-- name: IsAppIdle :one
-- The same test for one app, used by the sleep job to re-check before stopping.
SELECT EXISTS (
  SELECT 1 FROM apps a
  WHERE a.id = @id AND a.deleted_at IS NULL AND a.status = 'running' AND a.desired_state = 'running'
    AND a.current_deploy_id IS NOT NULL
    AND greatest(
          coalesce(a.last_request_at, '-infinity'::timestamptz),
          coalesce((SELECT max(i.created_at) FROM instances i
                    WHERE i.deploy_id = a.current_deploy_id AND i.state = 'running'),
                   '-infinity'::timestamptz)
        ) < now() - a.idle_timeout_s * interval '1 second'
)::boolean AS idle;
