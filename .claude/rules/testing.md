# Testing rules

- New behaviour needs tests. A bug fix starts with a failing test that reproduces it.
- Unit tests: `make test` (plain `go test ./...`, no Docker needed).
- Integration tests: files carry `//go:build integration`; run with `make test-integration`.
  They use real Postgres/Docker through `testcontainers-go`. Do not mock the database.
- Security-relevant changes add or update a test in `security/` that asserts the attack fails.
- Prefer table-driven tests; keep helpers in the test file unless two packages need them.
- Sample apps live in `testdata/apps/` (`hello-python`, `hello-node`, `static-site`, `whoami`, `evil-*`). Reuse them.
- Job handlers are idempotent: test that running one twice is safe.
- Performance-relevant changes: measure and record the number in `docs/PROGRESS.md` (Benchmarks table).
- Tests must pass before a commit; never commit a failing or skipped-to-green test.
