package domain

import "testing"

func TestIsTransientError(t *testing.T) {
	transient := []string{
		"dial tcp 10.0.0.1:5432: connection refused",
		"i/o timeout waiting for reply",
		"context deadline exceeded",
		"HTTP 503 service unavailable",
		"vm provisioning failed after 30s",
		"docker: error response from daemon: pull access denied",
	}
	permanent := []string{
		"assertion failed: expected 1, got 2",
		"TypeError: cannot read property 'x' of undefined",
		"exit status 1",
		"",
	}
	for _, e := range transient {
		if !IsTransientError(e) {
			t.Errorf("expected transient: %q", e)
		}
	}
	for _, e := range permanent {
		if IsTransientError(e) {
			t.Errorf("expected permanent: %q", e)
		}
	}
}

func TestTryRetry_BudgetAndClassification(t *testing.T) {
	run := &TestRun{MaxRetries: 2, Status: TestRunStatusRunning}

	if !run.TryRetry("connection refused") {
		t.Fatal("expected first transient retry to succeed")
	}
	if run.RetryCount != 1 {
		t.Fatalf("RetryCount=%d, want 1", run.RetryCount)
	}
	if run.Status != TestRunStatusRunning {
		t.Fatalf("Status=%s, want running after retry", run.Status)
	}
	if !run.TryRetry("i/o timeout") {
		t.Fatal("expected second retry to succeed")
	}
	if run.TryRetry("i/o timeout") {
		t.Fatal("expected third retry to fail (budget exhausted)")
	}

	run2 := &TestRun{MaxRetries: 2}
	if run2.TryRetry("assertion failed") {
		t.Fatal("expected permanent error to skip retry")
	}
	if run2.RetryCount != 0 {
		t.Fatalf("RetryCount=%d, want 0", run2.RetryCount)
	}
}

func TestTryRetry_DefaultMaxWhenZero(t *testing.T) {
	run := &TestRun{} // MaxRetries == 0 → should fall back to default
	for i := 0; i < DefaultMaxTestRetries; i++ {
		if !run.TryRetry("connection reset by peer") {
			t.Fatalf("retry %d expected to succeed", i)
		}
	}
	if run.TryRetry("connection reset by peer") {
		t.Fatal("expected retry beyond default cap to fail")
	}
}
