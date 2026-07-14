// #fleet-scale-to-zero-activity-feed (D-122, Option A): AccessLogFeed tails the
// fleet Caddy server's JSON access log and maintains an in-memory
// map[host]last-seen. SweepIdle consults it so an app served DIRECTLY through
// Caddy (bypassing the runtime activator that normally stamps LastActiveAt) is
// not wrongly scaled to zero. The hot request path is untouched — this is a
// read-only tail of the log file Caddy already writes.
package caddy

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"

	"github.com/sentiae/platform-kit/logger"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// defaultFeedPollInterval is how often the feed re-reads appended access-log
// lines when no interval is configured.
const defaultFeedPollInterval = time.Second

// AccessLogFeed tails a Caddy JSON access-log file and reports the last time
// each request host was seen. It is warm once it has successfully read the file
// at least once; until then (or if the file is unreadable) SweepIdle fails safe.
type AccessLogFeed struct {
	path     string
	interval time.Duration

	mu   sync.RWMutex
	seen map[string]time.Time
	warm bool

	// offset is the byte position up to which complete lines have been consumed.
	// Touched only by the run goroutine (poll).
	offset int64
}

var _ usecase.ActivityFeed = (*AccessLogFeed)(nil)

// NewAccessLogFeed constructs a feed tailing path. A non-positive interval falls
// back to defaultFeedPollInterval.
func NewAccessLogFeed(path string, interval time.Duration) *AccessLogFeed {
	if interval <= 0 {
		interval = defaultFeedPollInterval
	}
	return &AccessLogFeed{path: path, interval: interval, seen: map[string]time.Time{}}
}

// LastActivity returns the last time host was seen in the access log and whether
// it has been observed at all.
func (a *AccessLogFeed) LastActivity(host string) (time.Time, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	t, ok := a.seen[host]
	return t, ok
}

// Warm reports whether the feed has ingested the access log at least once. A
// cold feed (file not yet created, unreadable) stays false, making SweepIdle
// fail safe (treat every app as active).
func (a *AccessLogFeed) Warm() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.warm
}

// Run tails the access log until ctx is cancelled. ctx-aware + panic-recovering
// (constitution §30 rule 4/9). It polls immediately, then every interval.
func (a *AccessLogFeed) Run(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			logger.FromContext(ctx).Error("fleet activity feed: recovered from panic", "err", r)
		}
	}()
	a.poll(ctx)
	t := time.NewTicker(a.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.poll(ctx)
		}
	}
}

// poll reads newly-appended complete lines from the access log and folds them
// into the per-host last-seen map. It reads from the byte offset it left off at;
// a file shorter than that offset (rotation/truncation) resets to the start. A
// missing/unreadable file leaves the feed cold (warm untouched) — the fail-safe
// path in SweepIdle then treats apps as active.
func (a *AccessLogFeed) poll(ctx context.Context) {
	fi, err := os.Stat(a.path)
	if err != nil {
		return
	}
	if fi.Size() < a.offset {
		a.offset = 0
	}
	f, err := os.Open(a.path)
	if err != nil {
		logger.FromContext(ctx).Warn("fleet activity feed: open access log", "path", a.path, "err", err)
		return
	}
	defer f.Close()
	if a.offset > 0 {
		if _, serr := f.Seek(a.offset, io.SeekStart); serr != nil {
			a.offset = 0
		}
	}

	updates := map[string]time.Time{}
	r := bufio.NewReader(f)
	for {
		line, rerr := r.ReadBytes('\n')
		// Only a newline-terminated line is complete; a partial trailing line
		// (rerr == io.EOF, no '\n') is left for the next poll — offset is not
		// advanced past it.
		if rerr == nil {
			a.offset += int64(len(line))
			if host, ts, ok := parseAccessLine(line); ok {
				if cur, seen := updates[host]; !seen || ts.After(cur) {
					updates[host] = ts
				}
			}
			continue
		}
		break
	}
	a.apply(updates)
}

// apply folds a batch of per-host observations into the shared map (max wins)
// and marks the feed warm — reached only after a successful file read.
func (a *AccessLogFeed) apply(updates map[string]time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for h, t := range updates {
		if cur, ok := a.seen[h]; !ok || t.After(cur) {
			a.seen[h] = t
		}
	}
	a.warm = true
}

// accessLogLine is the subset of a Caddy JSON access-log entry the feed needs:
// the epoch-seconds timestamp (Caddy's default numeric "ts") and request host.
type accessLogLine struct {
	TS      float64 `json:"ts"`
	Request struct {
		Host string `json:"host"`
	} `json:"request"`
}

// parseAccessLine extracts (host, timestamp) from one Caddy JSON access-log
// line. ok is false for a non-JSON line or one without a request host.
func parseAccessLine(line []byte) (string, time.Time, bool) {
	var e accessLogLine
	if err := json.Unmarshal(line, &e); err != nil {
		return "", time.Time{}, false
	}
	if e.Request.Host == "" {
		return "", time.Time{}, false
	}
	sec := int64(e.TS)
	nsec := int64((e.TS - float64(sec)) * 1e9)
	return e.Request.Host, time.Unix(sec, nsec).UTC(), true
}
