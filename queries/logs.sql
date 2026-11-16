-- name: AddDeployLogLines :exec
-- Appends lines, in order, to a deploy's log. The app id comes from the deploy.
INSERT INTO log_lines (app_id, deploy_id, stream, line)
SELECT d.app_id, d.id, @stream::text, l.line
FROM deploys d, unnest(@lines::text[]) WITH ORDINALITY AS l(line, n)
WHERE d.id = @deploy_id
ORDER BY l.n;

-- name: ListDeployLogs :many
SELECT id, stream, line, ts FROM log_lines
WHERE deploy_id = @deploy_id AND id > @after
ORDER BY id
LIMIT 500;

-- name: GetOwnedDeployForLogs :one
SELECT d.id, d.state FROM deploys d JOIN apps a ON a.id = d.app_id
WHERE a.slug = @slug AND a.owner_id = @owner_id AND a.deleted_at IS NULL AND d.seq = @seq;

-- name: GetDeployState :one
SELECT state FROM deploys WHERE id = @id;
