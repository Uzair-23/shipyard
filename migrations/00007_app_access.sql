-- +goose Up
ALTER TABLE apps
    ADD COLUMN visibility text NOT NULL DEFAULT 'private' CHECK (visibility IN ('private', 'public'));

-- Members are identified by email, so an invite works before the person signs up.
CREATE TABLE app_members (
    app_id     uuid NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    email      citext NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (app_id, email)
);
CREATE INDEX app_members_email_idx ON app_members (email);

-- +goose Down
DROP TABLE app_members;
ALTER TABLE apps DROP COLUMN visibility;
