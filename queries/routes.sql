-- name: ListRoutes :many
-- One row per live app: the loopback port of the running instance of its
-- current deploy (0 when nothing is running). The router's route table.
SELECT a.id AS app_id, a.slug, a.status, a.visibility, coalesce(i.host_port, 0)::int AS host_port
FROM apps a
LEFT JOIN LATERAL (
    SELECT host_port FROM instances
    WHERE deploy_id = a.current_deploy_id AND state = 'running'
    ORDER BY created_at DESC
    LIMIT 1
) i ON true
WHERE a.deleted_at IS NULL;

-- name: GetRoute :one
SELECT a.id AS app_id, a.slug, a.status, a.visibility, coalesce(i.host_port, 0)::int AS host_port
FROM apps a
LEFT JOIN LATERAL (
    SELECT host_port FROM instances
    WHERE deploy_id = a.current_deploy_id AND state = 'running'
    ORDER BY created_at DESC
    LIMIT 1
) i ON true
WHERE a.id = @id AND a.deleted_at IS NULL;

-- name: NotifyRouteChange :exec
-- Tells routers to reload one app's route. Delivered on commit when sent in a transaction.
SELECT pg_notify('route_changes', sqlc.arg(app_id)::uuid::text);

-- name: TouchApps :exec
-- Batched last_request_at update from the router.
UPDATE apps SET last_request_at = v.ts
FROM (SELECT unnest(sqlc.arg(ids)::uuid[]) AS id, unnest(sqlc.arg(times)::timestamptz[]) AS ts) v
WHERE apps.id = v.id AND (apps.last_request_at IS NULL OR apps.last_request_at < v.ts);
