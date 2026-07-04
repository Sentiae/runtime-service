package compiler

import (
	"reflect"
	"testing"

	"github.com/sentiae/runtime-service/internal/domain"
)

func TestParseGoDiagnostics(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   []domain.CompileDiagnostic
	}{
		{
			name:   "empty output",
			output: "",
			want:   nil,
		},
		{
			name:   "single error",
			output: "internal/domain/user.go:12:9: undefined: Email",
			want: []domain.CompileDiagnostic{
				{File: "internal/domain/user.go", Line: 12, Column: 9, Message: "undefined: Email"},
			},
		},
		{
			name: "multiple errors plus noise",
			output: "# example.com/m/internal/domain\n" +
				"internal/domain/user.go:12:9: undefined: Email\n" +
				"main.go:3:8: \"fmt\" imported and not used\n",
			want: []domain.CompileDiagnostic{
				{File: "internal/domain/user.go", Line: 12, Column: 9, Message: "undefined: Email"},
				{File: "main.go", Line: 3, Column: 8, Message: "\"fmt\" imported and not used"},
			},
		},
		{
			name:   "carriage returns trimmed",
			output: "main.go:1:1: syntax error\r",
			want: []domain.CompileDiagnostic{
				{File: "main.go", Line: 1, Column: 1, Message: "syntax error"},
			},
		},
		{
			name:   "non-go line ignored",
			output: "go: downloading github.com/google/uuid v1.6.0",
			want:   nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseGoDiagnostics(tt.output)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseGoDiagnostics() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParseTSDiagnostics(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   []domain.CompileDiagnostic
	}{
		{
			name:   "empty output",
			output: "",
			want:   nil,
		},
		{
			name:   "single error",
			output: "src/index.ts(10,5): error TS2304: Cannot find name 'foo'.",
			want: []domain.CompileDiagnostic{
				{File: "src/index.ts", Line: 10, Column: 5, Message: "Cannot find name 'foo'."},
			},
		},
		{
			name: "multiple errors",
			output: "src/a.ts(1,1): error TS1005: ';' expected.\n" +
				"src/b.ts(42,13): error TS2322: Type 'string' is not assignable to type 'number'.\n",
			want: []domain.CompileDiagnostic{
				{File: "src/a.ts", Line: 1, Column: 1, Message: "';' expected."},
				{File: "src/b.ts", Line: 42, Column: 13, Message: "Type 'string' is not assignable to type 'number'."},
			},
		},
		{
			name:   "non-error tsc line ignored",
			output: "Files: 12\nLines: 3400",
			want:   nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseTSDiagnostics(tt.output)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseTSDiagnostics() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestToolchainFor(t *testing.T) {
	tests := []struct {
		name      string
		language  string
		wantOK    bool
		wantImage string
	}{
		{"go", "go", true, "golang:1.25-alpine"},
		{"golang alias", "golang", true, "golang:1.25-alpine"},
		{"typescript", "typescript", true, "node:22-alpine"},
		{"ts alias", "ts", true, "node:22-alpine"},
		{"unsupported", "rust", false, ""},
		{"empty", "", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc, ok := toolchainFor(tt.language)
			if ok != tt.wantOK {
				t.Fatalf("toolchainFor(%q) ok = %v, want %v", tt.language, ok, tt.wantOK)
			}
			if ok && tc.image != tt.wantImage {
				t.Fatalf("toolchainFor(%q) image = %q, want %q", tt.language, tc.image, tt.wantImage)
			}
		})
	}
}

// TestCompile_NoDocker verifies the compiler degrades to a toolchain error
// rather than invoking docker when the CLI is unresolved. This guards the
// unit suite from ever shelling out to a real build.
func TestCompile_NoDocker(t *testing.T) {
	c := &DockerCompiler{dockerPath: ""}
	_, err := c.Compile(t.Context(), "go", []domain.SourceFile{{Path: "main.go", Content: "package main"}}, 5)
	if err != domain.ErrCompileToolchainUnavailable {
		t.Fatalf("expected ErrCompileToolchainUnavailable, got %v", err)
	}
}

// TestCompile_UnsupportedLanguage rejects before any docker interaction.
func TestCompile_UnsupportedLanguage(t *testing.T) {
	c := &DockerCompiler{dockerPath: ""}
	_, err := c.Compile(t.Context(), "rust", []domain.SourceFile{{Path: "main.rs", Content: ""}}, 5)
	if err != domain.ErrUnsupportedCompileLanguage {
		t.Fatalf("expected ErrUnsupportedCompileLanguage, got %v", err)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("abcdef", 3); got != "abc" {
		t.Fatalf("truncate = %q, want abc", got)
	}
	if got := truncate("ab", 5); got != "ab" {
		t.Fatalf("truncate = %q, want ab", got)
	}
}

func TestClampTimeout(t *testing.T) {
	if got := clampTimeout(0); got != defaultTimeoutSec {
		t.Fatalf("clampTimeout(0) = %d, want %d", got, defaultTimeoutSec)
	}
	if got := clampTimeout(9999); got != maxTimeoutSec {
		t.Fatalf("clampTimeout(9999) = %d, want %d", got, maxTimeoutSec)
	}
	if got := clampTimeout(30); got != 30 {
		t.Fatalf("clampTimeout(30) = %d, want 30", got)
	}
}
