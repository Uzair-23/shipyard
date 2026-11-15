-- name: EnqueueJob :one
INSERT INTO jobs (kind, app_id, payload, max_attempts)
VALUES (@kind, @app_id, @payload, @max_attempts)
RETURNING *;

-- name: NotifyJobsNew :exec
SELECT pg_notify('jobs_new', '');

-- name: ClaimJob :one
-- Takes the oldest due queued job, or a running job whose lease expired
-- (its worker died). A reclaim increments reclaims, not attempts (ADR 011).
UPDATE jobs
SET state        = 'running',
    locked_by    = @worker_id::text,
    locked_until = now() + sqlc.arg(lease_secs)::float8 * interval '1 second',
    reclaims     = reclaims + CASE WHEN state = 'running' THEN 1 ELSE 0 END
WHERE id = (
    SELECT j.id FROM jobs j
    WHERE j.kind = ANY (@kinds::text[])
      AND ((j.state = 'queued' AND j.run_after <= now())
        OR (j.state = 'running' AND j.locked_until < now()))
    ORDER BY j.created_at
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING *;

-- name: ExtendJobLease :execrows
UPDATE jobs
SET locked_until = now() + sqlc.arg(lease_secs)::float8 * interval '1 second'
WHERE id = @id AND state = 'running' AND locked_by = @worker_id::text;

-- name: CompleteJob :execrows
UPDATE jobs
SET state = 'done', finished_at = now(), locked_by = NULL, locked_until = NULL
WHERE id = @id AND state = 'running' AND locked_by = @worker_id::text;

-- name: FailJob :execrows
-- Records a handler failure: retry after a backoff, or fail for good once
-- max_attempts is reached.
UPDATE jobs
SET attempts     = attempts + 1,
    last_error   = @last_error::text,
    state        = CASE WHEN attempts + 1 >= max_attempts THEN 'failed' ELSE 'queued' END,
    finished_at  = CASE WHEN attempts + 1 >= max_attempts THEN now() END,
    run_after    = now() + sqlc.arg(backoff_secs)::float8 * interval '1 second',
    locked_by    = NULL,
    locked_until = NULL
WHERE id = @id AND state = 'running' AND locked_by = @worker_id::text;

-- name: FailJobPermanently :execrows
UPDATE jobs
SET state = 'failed', last_error = @last_error::text, finished_at = now(), locked_by = NULL, locked_until = NULL
WHERE id = @id AND state = 'running' AND locked_by = @worker_id::text;

-- name: ReleaseJob :execrows
-- Puts a job back without counting a failure (worker shutdown).
UPDATE jobs
SET state = 'queued', run_after = now(), locked_by = NULL, locked_until = NULL
WHERE id = @id AND state = 'running' AND locked_by = @worker_id::text;
