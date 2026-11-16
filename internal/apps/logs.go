package apps

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Uzair-23/shipyard/internal/store"
)

const (
	// maxDeployLogLines caps what one build can write to log_lines.
	maxDeployLogLines = 5000
	logFlushInterval  = 300 * time.Millisecond
)

type logEntry struct{ stream, line string }

// logSink collects a deploy's log lines and writes them to log_lines in
// batches, so a chatty build costs a few inserts a second, not one per line.
// Failing to store logs never fails a build.
type logSink struct {
	pool   *pgxpool.Pool
	deploy pgtype.UUID

	mu      sync.Mutex
	pending []logEntry
	total   int
}

func newLogSink(pool *pgxpool.Pool, deploy pgtype.UUID) *logSink {
	return &logSink{pool: pool, deploy: deploy}
}

// Add queues a line. Beyond maxDeployLogLines one notice is stored and the rest dropped.
func (s *logSink) Add(stream, line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total++
	switch {
	case s.total < maxDeployLogLines:
		s.pending = append(s.pending, logEntry{stream, line})
	case s.total == maxDeployLogLines:
		s.pending = append(s.pending, logEntry{"system", "(log truncated: too many lines)"})
	}
}

// Build is the callback for Builder.BuildLogged.
func (s *logSink) Build(line string) { s.Add("build", line) }

func (s *logSink) flush(ctx context.Context) {
	s.mu.Lock()
	batch := s.pending
	s.pending = nil
	s.mu.Unlock()
	q := store.New(s.pool)
	for len(batch) > 0 { // one insert per run of lines of the same stream, in order
		n := 1
		for n < len(batch) && batch[n].stream == batch[0].stream {
			n++
		}
		lines := make([]string, n)
		for i := range lines {
			lines[i] = batch[i].line
		}
		if err := q.AddDeployLogLines(ctx, store.AddDeployLogLinesParams{Stream: batch[0].stream, Lines: lines, DeployID: s.deploy}); err != nil {
			slog.Warn("store build log lines failed", "deploy_id", s.deploy.String(), "error", err)
		}
		batch = batch[n:]
	}
}

// Start flushes in the background until the returned stop function is called,
// which writes whatever is left.
func (s *logSink) Start(ctx context.Context) (stop func()) {
	bg, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(logFlushInterval)
		defer t.Stop()
		for {
			select {
			case <-bg.Done():
				return
			case <-t.C:
				s.flush(bg)
			}
		}
	}()
	return func() {
		cancel()
		<-done
		s.flush(context.WithoutCancel(ctx))
	}
}

// logSystem appends platform messages to a deploy's log inside the caller's
// transaction (or pool), so they become visible together with the state change.
func logSystem(ctx context.Context, db store.DBTX, deploy pgtype.UUID, lines ...string) error {
	if err := store.New(db).AddDeployLogLines(ctx, store.AddDeployLogLinesParams{Stream: "system", Lines: lines, DeployID: deploy}); err != nil {
		return fmt.Errorf("store log line: %w", err)
	}
	return nil
}

// firstLine is the headline of a multi-line failure message.
func firstLine(msg string) string {
	line, _, _ := strings.Cut(msg, "\n")
	return strings.TrimSpace(line)
}
