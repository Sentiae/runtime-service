package usecase

import "testing"

func TestPatternMatcher_CRUDOnRepository(t *testing.T) {
	m := NewPatternMatcher()
	src := `
func (s *UserService) CreateUser(ctx context.Context, repo *UserRepository, input CreateUserInput) (*User, error) {
	u := &User{Name: input.Name}
	return repo.Create(ctx, u)
}

func (s *UserService) UpdateUser(ctx context.Context, id uuid.UUID, input UpdateUserInput) (*User, error) {
	u, err := s.repo.Update(ctx, id, input)
	return u, err
}
`
	m2 := m.Match(src)
	if m2.Kind != PatternKindCRUD {
		t.Fatalf("expected CRUD, got %s (conf %.2f)", m2.Kind, m2.Confidence)
	}
	if m2.Confidence < 0.6 {
		t.Fatalf("expected confidence >= 0.6, got %.2f", m2.Confidence)
	}
	if m2.Kind.AsTestCategory() != TestCategoryCRUD {
		t.Fatalf("expected TestCategoryCRUD, got %s", m2.Kind.AsTestCategory())
	}
}

func TestPatternMatcher_AuthCheck(t *testing.T) {
	m := NewPatternMatcher()
	src := `
func RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Authorization")
		claims, err := jwt.Parse(token, keyfunc)
		if err != nil || !HasPermission(claims, "users:read") {
			http.Error(w, "forbidden", 403)
			return
		}
		next.ServeHTTP(w, r)
	})
}
`
	m2 := m.Match(src)
	if m2.Kind != PatternKindAuth {
		t.Fatalf("expected Auth, got %s (conf %.2f)", m2.Kind, m2.Confidence)
	}
	if m2.Confidence < 0.6 {
		t.Fatalf("expected confidence >= 0.6, got %.2f", m2.Confidence)
	}
	if m2.Kind.AsTestCategory() != TestCategoryAuth {
		t.Fatalf("expected TestCategoryAuth, got %s", m2.Kind.AsTestCategory())
	}
}

func TestPatternMatcher_ValidationError(t *testing.T) {
	m := NewPatternMatcher()
	src := `
def create_user(name: str, age: int):
    if name == "":
        raise ValueError("name is required")
    if age < 0:
        raise ValidationError("age must be >= 0")
    if len(name) == 0:
        raise ValueError("empty name")
    return User(name=name, age=age)
`
	m2 := m.Match(src)
	if m2.Kind != PatternKindValidation {
		t.Fatalf("expected Validation, got %s (conf %.2f)", m2.Kind, m2.Confidence)
	}
	// Validation collapses to smoke for now — verifying the taxonomy
	// mapping here makes downstream behaviour explicit.
	if m2.Kind.AsTestCategory() != TestCategorySmoke {
		t.Fatalf("validation should map to smoke, got %s", m2.Kind.AsTestCategory())
	}
}

func TestPatternMatcher_UnknownFallsBack(t *testing.T) {
	m := NewPatternMatcher()
	// Purely mathematical code has no CRUD / auth / validation signal.
	src := `
func add(a, b int) int { return a + b }
func square(x int) int { return x * x }
`
	m2 := m.Match(src)
	if m2.Kind != PatternKindUnknown {
		t.Fatalf("expected Unknown, got %s (conf %.2f)", m2.Kind, m2.Confidence)
	}
}

func TestClassifyWithPattern_HighConfidencePrefersPattern(t *testing.T) {
	m := NewPatternMatcher()
	src := `
func (r *WidgetRepository) Create(w *Widget) error { return r.db.Create(w).Error }
func (r *WidgetRepository) Delete(id uuid.UUID) error { return r.db.Delete(&Widget{}, id).Error }
func (r *WidgetRepository) Update(w *Widget) error { return r.db.Save(w).Error }
`
	// Keyword classifier would say "auth" because of the misleading
	// word in criteria — we want the pattern matcher to win.
	cat := ClassifyWithPattern(m, src, "auth flow", []string{"login"}, 0.6)
	if cat != TestCategoryCRUD {
		t.Fatalf("expected CRUD override, got %s", cat)
	}
}

func TestClassifyWithPattern_LowConfidenceFallsBack(t *testing.T) {
	m := NewPatternMatcher()
	// No actionable pattern in the source; keyword classifier should
	// take over.
	src := `func helloWorld() string { return "hello" }`
	cat := ClassifyWithPattern(m, src, "login and session management", []string{"OAuth token flow"}, 0.6)
	if cat != TestCategoryAuth {
		t.Fatalf("expected keyword fallback to Auth, got %s", cat)
	}
}

func TestClassifyWithPattern_NoSourceFallsBack(t *testing.T) {
	m := NewPatternMatcher()
	// No source supplied — keyword classifier runs.
	cat := ClassifyWithPattern(m, "", "CRUD endpoints", []string{"create, read, update, delete entity"}, 0.6)
	if cat != TestCategoryCRUD {
		t.Fatalf("expected keyword fallback to CRUD, got %s", cat)
	}
}
