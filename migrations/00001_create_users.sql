-- +goose Up
CREATE EXTENSION IF NOT EXISTS citext;

CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email         citext NOT NULL UNIQUE,
    password_hash text,
    github_id     bigint UNIQUE,
    is_admin      boolean NOT NULL DEFAULT false,
    is_trusted    boolean NOT NULL DEFAULT false,
    suspended_at  timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE users;
