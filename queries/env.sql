-- name: ListEnv :many
SELECT key, value_encrypted, nonce, is_secret, updated_at FROM env_vars WHERE app_id = @app_id ORDER BY key;

-- name: CountEnv :one
SELECT count(*) FROM env_vars WHERE app_id = @app_id;

-- name: EnvExists :one
SELECT EXISTS (SELECT 1 FROM env_vars WHERE app_id = @app_id AND key = @key);

-- name: UpsertEnv :exec
INSERT INTO env_vars (app_id, key, value_encrypted, nonce, is_secret)
VALUES (@app_id, @key, @value_encrypted, @nonce, @is_secret)
ON CONFLICT (app_id, key) DO UPDATE
SET value_encrypted = EXCLUDED.value_encrypted, nonce = EXCLUDED.nonce,
    is_secret = EXCLUDED.is_secret, updated_at = now();

-- name: DeleteEnv :execrows
DELETE FROM env_vars WHERE app_id = @app_id AND key = @key;
