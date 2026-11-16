-- +goose Up
-- At most one live deploy per app, enforced by the database (security review
-- of M1 task 10): concurrent or out-of-order promotions cannot leave two.
CREATE UNIQUE INDEX deploys_one_live_per_app ON deploys (app_id) WHERE state = 'live';

-- +goose Down
DROP INDEX deploys_one_live_per_app;
