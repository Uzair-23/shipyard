-- +goose Up
-- One-time codes that carry a logged-in user from the dashboard to an app's host (ADR 005).
CREATE TABLE handoff_codes (
    code_hash   bytea PRIMARY KEY,
    app_id      uuid NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    return_path text NOT NULL,
    expires_at  timestamptz NOT NULL,
    used_at     timestamptz
);
CREATE INDEX handoff_codes_app_id_idx ON handoff_codes (app_id);
CREATE INDEX handoff_codes_user_id_idx ON handoff_codes (user_id);
CREATE INDEX handoff_codes_expires_at_idx ON handoff_codes (expires_at);

-- +goose Down
DROP TABLE handoff_codes;
