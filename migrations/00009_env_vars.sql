-- +goose Up
-- Every value is encrypted (AES-256-GCM, bound to app and name). is_secret only
-- decides whether the API ever shows the value again.
CREATE TABLE env_vars (
    app_id          uuid NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    key             text NOT NULL,
    value_encrypted bytea NOT NULL,
    nonce           bytea NOT NULL,
    is_secret       boolean NOT NULL DEFAULT false,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (app_id, key)
);

-- +goose Down
DROP TABLE env_vars;
