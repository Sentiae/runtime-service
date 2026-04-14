package usecase

import (
	"testing"

	"github.com/sentiae/runtime-service/internal/domain"
)

func TestResolveTestRunner_ExactMatch(t *testing.T) {
	p, matched := ResolveTestRunner(domain.Language("typescript"), domain.TestTypeVisual)
	if !matched {
		t.Fatal("expected exact match for typescript/visual")
	}
	if p.VMProfile != "large" || !p.Network {
		t.Fatalf("unexpected profile: %+v", p)
	}
}

func TestResolveTestRunner_FallbackToUnit(t *testing.T) {
	p, matched := ResolveTestRunner(domain.Language("rust"), domain.TestTypeA11y)
	if matched {
		t.Fatal("rust has no a11y runner — expected fallback")
	}
	if p.Command == "" || p.VMProfile == "" {
		t.Fatalf("fallback returned empty profile: %+v", p)
	}
}

func TestResolveTestRunner_UnknownLanguage(t *testing.T) {
	p, matched := ResolveTestRunner(domain.Language("cobol"), domain.TestTypeUnit)
	if matched {
		t.Fatal("cobol is not in table — expected !matched")
	}
	if p.Command == "" {
		t.Fatal("unknown language should still return a non-empty default")
	}
}

func TestResolveTestRunner_NormalizesInvalidType(t *testing.T) {
	p, _ := ResolveTestRunner(domain.Language("go"), domain.TestType("unknown"))
	if p.Command != "go test ./..." {
		t.Fatalf("expected normalization to unit, got %q", p.Command)
	}
}
