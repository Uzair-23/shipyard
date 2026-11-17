-- +goose Up
-- How long the last wake of a sleeping app took (router enqueue to a running route), in ms.
ALTER TABLE apps ADD COLUMN last_cold_start_ms integer;

-- +goose Down
ALTER TABLE apps DROP COLUMN last_cold_start_ms;
