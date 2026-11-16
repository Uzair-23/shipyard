-- name: CreateUser :one
INSERT INTO users (email, password_hash) VALUES (@email, @password_hash)
RETURNING id, email, created_at;

-- name: CreateSession :exec
INSERT INTO sessions (token_hash, user_id, expires_at) VALUES (@token_hash, @user_id, @expires_at);

-- name: GetSessionUser :one
-- The user behind a live session; suspended users have no session.
SELECT u.id, u.email
FROM sessions s JOIN users u ON u.id = s.user_id
WHERE s.token_hash = @token_hash AND s.expires_at > now() AND u.suspended_at IS NULL;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE token_hash = @token_hash;
