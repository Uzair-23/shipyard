-- +goose Up
-- Postgres-backed job queue (ADR 002), with leases per ADR 011.
CREATE TABLE jobs (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind         text NOT NULL,
    app_id       uuid REFERENCES apps (id) ON DELETE CASCADE,
    payload      jsonb NOT NULL DEFAULT '{}',
    state        text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'running', 'done', 'failed')),
    attempts     integer NOT NULL DEFAULT 0, -- handler failures
    max_attempts integer NOT NULL DEFAULT 1 CHECK (max_attempts >= 1),
    reclaims     integer NOT NULL DEFAULT 0, -- lease expiries (worker died)
    run_after    timestamptz NOT NULL DEFAULT now(),
    locked_by    text,
    locked_until timestamptz,
    last_error   text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    finished_at  timestamptz
);
CREATE INDEX jobs_app_id_idx ON jobs (app_id);
-- Claim path: queued jobs that are due, oldest first.
CREATE INDEX jobs_queued_idx ON jobs (created_at) WHERE state = 'queued';
-- Reclaim path: running jobs whose lease may have expired.
CREATE INDEX jobs_running_lease_idx ON jobs (locked_until) WHERE state = 'running';

-- +goose Down
DROP TABLE jobs;
