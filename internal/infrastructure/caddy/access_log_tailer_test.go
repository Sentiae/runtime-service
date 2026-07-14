package caddy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mkLine(t *testing.T, host string, ts float64) string {
	t.Helper()
	m := map[string]any{
		"level":   "info",
		"ts":      ts,
		"logger":  "http.log.access.fleet_access",
		"msg":     "handled request",
		"request": map[string]any{"host": host, "method": "GET", "uri": "/"},
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal line: %v", err)
	}
	return string(b)
}

func TestParseAccessLine(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		wantHost string
		wantTS   time.Time
		wantOK   bool
	}{
		{
			name:     "valid entry",
			line:     mkLine(t, "app-prod.fleet.sentiae.local", 1_700_000_000.5),
			wantHost: "app-prod.fleet.sentiae.local",
			wantTS:   time.Unix(1_700_000_000, 500_000_000).UTC(),
			wantOK:   true,
		},
		{"non-json", "not-json", "", time.Time{}, false},
		{"no host", `{"ts":1700000000,"request":{"method":"GET"}}`, "", time.Time{}, false},
		{"empty", "", "", time.Time{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, ts, ok := parseAccessLine([]byte(tt.line))
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if host != tt.wantHost {
				t.Fatalf("host = %q, want %q", host, tt.wantHost)
			}
			if !ts.Equal(tt.wantTS) {
				t.Fatalf("ts = %v, want %v", ts, tt.wantTS)
			}
		})
	}
}

func TestAccessLogFeedPoll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "caddy-access.log")

	h1 := time.Unix(1_700_000_100, 0).UTC()
	h2b := time.Unix(1_700_000_200, 0).UTC()
	content := mkLine(t, "one.fleet.sentiae.local", 1_700_000_100) + "\n" +
		mkLine(t, "two.fleet.sentiae.local", 1_700_000_050) + "\n" +
		mkLine(t, "two.fleet.sentiae.local", 1_700_000_200) + "\n" +
		"garbage-not-json\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	feed := NewAccessLogFeed(path, time.Second)
	if feed.Warm() {
		t.Fatalf("feed warm before first poll")
	}
	feed.poll(context.Background())
	if !feed.Warm() {
		t.Fatalf("feed not warm after poll of an existing file")
	}

	if got, ok := feed.LastActivity("one.fleet.sentiae.local"); !ok || !got.Equal(h1) {
		t.Fatalf("host one = (%v,%v), want (%v,true)", got, ok, h1)
	}
	// Latest wins for a repeated host.
	if got, ok := feed.LastActivity("two.fleet.sentiae.local"); !ok || !got.Equal(h2b) {
		t.Fatalf("host two = (%v,%v), want (%v,true)", got, ok, h2b)
	}
	if _, ok := feed.LastActivity("unseen.fleet.sentiae.local"); ok {
		t.Fatalf("unseen host reported as seen")
	}

	// Appended lines are picked up incrementally without re-reading old ones.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := fmt.Fprintln(f, mkLine(t, "one.fleet.sentiae.local", 1_700_000_300)); err != nil {
		t.Fatalf("append: %v", err)
	}
	_ = f.Close()

	feed.poll(context.Background())
	want := time.Unix(1_700_000_300, 0).UTC()
	if got, ok := feed.LastActivity("one.fleet.sentiae.local"); !ok || !got.Equal(want) {
		t.Fatalf("host one after append = (%v,%v), want (%v,true)", got, ok, want)
	}
}

func TestAccessLogFeedMissingFileStaysCold(t *testing.T) {
	feed := NewAccessLogFeed(filepath.Join(t.TempDir(), "does-not-exist.log"), time.Second)
	feed.poll(context.Background())
	if feed.Warm() {
		t.Fatalf("feed warm despite missing access-log file")
	}
	if _, ok := feed.LastActivity("any.host"); ok {
		t.Fatalf("cold feed reported activity")
	}
}
