-- +goose Up
-- A deleted app releases its slug: uniqueness only applies to apps that are not deleted.
ALTER TABLE apps DROP CONSTRAINT apps_slug_key;
CREATE UNIQUE INDEX apps_slug_live_key ON apps (slug) WHERE deleted_at IS NULL;

-- +goose Down
DROP INDEX apps_slug_live_key;
ALTER TABLE apps ADD CONSTRAINT apps_slug_key UNIQUE (slug);
