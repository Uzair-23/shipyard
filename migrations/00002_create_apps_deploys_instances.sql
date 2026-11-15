-- +goose Up
CREATE TABLE apps (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Nullable until accounts exist (M3 makes it NOT NULL).
    owner_id          uuid REFERENCES users (id),
    slug              text NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9-]{3,30}$'),
    runtime           text CHECK (runtime IN ('python', 'node', 'static')),
    idle_timeout_s    integer NOT NULL DEFAULT 900 CHECK (idle_timeout_s BETWEEN 300 AND 3600),
    desired_state     text NOT NULL DEFAULT 'running' CHECK (desired_state IN ('running', 'stopped')),
    status            text NOT NULL DEFAULT 'stopped'
                      CHECK (status IN ('stopped', 'starting', 'running', 'sleeping', 'crashed')),
    current_deploy_id uuid,
    last_request_at   timestamptz,
    deleted_at        timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX apps_owner_id_idx ON apps (owner_id);

CREATE TABLE deploys (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    app_id          uuid NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    seq             integer NOT NULL,
    source_ref      text NOT NULL,
    config_snapshot jsonb NOT NULL DEFAULT '{}',
    image_digest    text,
    state           text NOT NULL DEFAULT 'queued'
                    CHECK (state IN ('queued', 'building', 'built', 'starting', 'health_check',
                                     'live', 'failed', 'superseded')),
    error           text,
    created_by      uuid REFERENCES users (id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    finished_at     timestamptz,
    UNIQUE (app_id, seq)
);
CREATE INDEX deploys_created_by_idx ON deploys (created_by);

ALTER TABLE apps
    ADD CONSTRAINT apps_current_deploy_id_fkey
    FOREIGN KEY (current_deploy_id) REFERENCES deploys (id) ON DELETE SET NULL;
CREATE INDEX apps_current_deploy_id_idx ON apps (current_deploy_id);

CREATE TABLE instances (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    deploy_id     uuid NOT NULL REFERENCES deploys (id) ON DELETE CASCADE,
    container_id  text,
    host_port     integer,
    state         text NOT NULL DEFAULT 'starting'
                  CHECK (state IN ('starting', 'running', 'stopped', 'failed')),
    started_at    timestamptz,
    stopped_at    timestamptz,
    exit_code     integer,
    restart_count integer NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX instances_deploy_id_idx ON instances (deploy_id);

-- +goose Down
DROP TABLE instances;
ALTER TABLE apps DROP CONSTRAINT apps_current_deploy_id_fkey;
DROP TABLE deploys;
DROP TABLE apps;
