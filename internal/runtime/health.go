package runtime

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Health check timing (ARCHITECTURE §8.2, PRD APP-8).
const (
	HealthInterval       = 100 * time.Millisecond
	HealthTimeout        = 30 * time.Second
	healthRequestTimeout = 2 * time.Second
)

// healthClient never follows redirects, so an app cannot point the check at
// another host. Its own transport bounds what an app can make the worker do:
// no proxy, no kept-alive connections, small response headers, timeouts.
var healthClient = &http.Client{
	Timeout: healthRequestTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
	Transport: &http.Transport{
		Proxy:                  nil,
		DialContext:            (&net.Dialer{Timeout: time.Second}).DialContext,
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: 16 << 10,
		ResponseHeaderTimeout:  healthRequestTimeout,
	},
}

// UnhealthyError means the app did not pass its health check in time. Its
// message is safe to show to the user.
type UnhealthyError struct {
	Path string
	Last string // last status or connection error seen
	Wait time.Duration
}

func (e *UnhealthyError) Error() string {
	return fmt.Sprintf("the app did not respond successfully on %s within %s (last result: %s); "+
		"check that it listens on 0.0.0.0:$PORT and that the health path returns a 2xx or 3xx status", e.Path, e.Wait, e.Last)
}

// WaitHealthy polls url every interval until it returns a 2xx or 3xx status,
// ctx ends, or timeout passes. path is only used in the error message.
func WaitHealthy(parent context.Context, url, path string, interval, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	t := time.NewTicker(interval)
	defer t.Stop()

	last := "no response yet"
	for {
		ok, result := probe(ctx, url)
		if ok {
			return nil
		}
		if ctx.Err() == nil { // a probe cut off by the deadline says nothing about the app
			last = result
		}
		select {
		case <-ctx.Done():
			if err := parent.Err(); err != nil {
				return err // caller cancelled (shutdown, lost lease): not the app's fault
			}
			return &UnhealthyError{Path: path, Last: last, Wait: timeout}
		case <-t.C:
		}
	}
}

func probe(ctx context.Context, url string) (bool, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, "invalid health URL"
	}
	req.Header.Set("User-Agent", "shipyard-health-check")
	resp, err := healthClient.Do(req)
	if err != nil {
		return false, "connection failed"
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return true, ""
	}
	return false, fmt.Sprintf("HTTP %d", resp.StatusCode)
}
