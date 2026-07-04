//go:build unit

package usecase

import (
	"testing"
)

func intPtr(n int) *int { return &n }

func TestShapeCodeOutput(t *testing.T) {
	tests := []struct {
		name     string
		stdout   string
		stderr   string
		exitCode *int
		wantErr  bool
		// assert checks fields on the produced output map.
		assert func(t *testing.T, out map[string]any)
	}{
		{
			name:     "plain non-json stdout, zero exit",
			stdout:   "hello world",
			stderr:   "",
			exitCode: intPtr(0),
			wantErr:  false,
			assert: func(t *testing.T, out map[string]any) {
				if out["stdout"] != "hello world" {
					t.Fatalf("stdout = %v", out["stdout"])
				}
				if out["output"] != "hello world" {
					t.Fatalf("output = %v", out["output"])
				}
			},
		},
		{
			name:     "json stdout merges keys",
			stdout:   `{"short_url":"abc","ok":true}`,
			stderr:   "",
			exitCode: intPtr(0),
			wantErr:  false,
			assert: func(t *testing.T, out map[string]any) {
				if out["short_url"] != "abc" {
					t.Fatalf("expected merged short_url, got %v", out["short_url"])
				}
				if out["ok"] != true {
					t.Fatalf("expected merged ok=true, got %v", out["ok"])
				}
				// Base field still present.
				if out["stdout"] != `{"short_url":"abc","ok":true}` {
					t.Fatalf("stdout overwritten: %v", out["stdout"])
				}
			},
		},
		{
			name:     "non-zero exit returns error and does not merge",
			stdout:   `{"x":1}`,
			stderr:   "boom",
			exitCode: intPtr(2),
			wantErr:  true,
			assert: func(t *testing.T, out map[string]any) {
				if _, merged := out["x"]; merged {
					t.Fatalf("must not merge stdout json on non-zero exit")
				}
				if out["stderr"] != "boom" {
					t.Fatalf("stderr = %v", out["stderr"])
				}
			},
		},
		{
			name:     "nil exit code treated as success",
			stdout:   "plain",
			stderr:   "",
			exitCode: nil,
			wantErr:  false,
			assert: func(t *testing.T, out map[string]any) {
				if out["exit_code"] != (*int)(nil) {
					t.Fatalf("exit_code = %v, want nil *int", out["exit_code"])
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := shapeCodeOutput(tt.stdout, tt.stderr, tt.exitCode)
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			tt.assert(t, out)
		})
	}
}
