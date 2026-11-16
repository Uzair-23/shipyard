-- +goose Up
-- Build output and platform messages per deploy (ADR 014: a plain table, no
-- partitions). Runtime stdout/stderr use the same table later.
CREATE TABLE log_lines (
    id        bigserial PRIMARY KEY,
    app_id    uuid NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    deploy_id uuid REFERENCES deploys (id) ON DELETE CASCADE,
    stream    text NOT NULL CHECK (stream IN ('build', 'system', 'stdout', 'stderr')),
    line      text NOT NULL,
    ts        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX log_lines_app_id_id_idx ON log_lines (app_id, id);
CREATE INDEX log_lines_deploy_id_id_idx ON log_lines (deploy_id, id);

-- +goose Down
DROP TABLE log_lines;
