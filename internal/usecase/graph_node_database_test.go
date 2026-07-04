package usecase

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
)

func TestExecuteDatabaseNode_ConfigValidation(t *testing.T) {
	e := &GraphExecutionEngine{}
	tests := []struct {
		name    string
		config  domain.JSONMap
		wantErr string
	}{
		{
			name:    "missing connection_string",
			config:  domain.JSONMap{"query": "SELECT 1"},
			wantErr: "connection_string is required",
		},
		{
			name:    "missing query",
			config:  domain.JSONMap{"connection_string": "postgres://x"},
			wantErr: "query is required",
		},
		{
			name:    "unsupported driver",
			config:  domain.JSONMap{"connection_string": "x", "query": "SELECT 1", "driver": "oracle"},
			wantErr: "unsupported database driver",
		},
		{
			name:    "params not an array",
			config:  domain.JSONMap{"connection_string": "x", "query": "SELECT 1", "params": "slug"},
			wantErr: "params must be an array",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &domain.GraphNode{ID: uuid.New(), Name: "db", NodeType: domain.GraphNodeTypeDatabase, Config: tt.config}
			_, err := e.executeDatabaseNode(context.Background(), node, domain.JSONMap{})
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("got error %q, want substring %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestDBStatementIsQuery(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  bool
	}{
		{"select", "SELECT * FROM links", true},
		{"select lowercase", "select id from links", true},
		{"select leading whitespace", "  \n SELECT 1", true},
		{"with cte", "WITH t AS (SELECT 1) SELECT * FROM t", true},
		{"insert returning", "INSERT INTO links (slug) VALUES ($1) RETURNING id", true},
		{"update returning lowercase", "update links set hits = hits + 1 where slug = $1 returning hits", true},
		{"plain insert", "INSERT INTO links (slug, url) VALUES ($1, $2)", false},
		{"plain update", "UPDATE links SET hits = hits + 1 WHERE slug = $1", false},
		{"plain delete", "DELETE FROM links WHERE slug = $1", false},
		{"insert with returning as substring not keyword", "INSERT INTO returning_audit (x) VALUES ($1)", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dbStatementIsQuery(tt.query); got != tt.want {
				t.Fatalf("dbStatementIsQuery(%q) = %v, want %v", tt.query, got, tt.want)
			}
		})
	}
}

func TestDBBindArgs(t *testing.T) {
	tests := []struct {
		name    string
		config  domain.JSONMap
		input   domain.JSONMap
		want    []any
		wantErr string
	}{
		{
			name:   "no params",
			config: domain.JSONMap{},
			input:  domain.JSONMap{"slug": "abc"},
			want:   nil,
		},
		{
			name:   "positional binding in order",
			config: domain.JSONMap{"params": []any{"slug", "url"}},
			input:  domain.JSONMap{"url": "https://x.com", "slug": "abc"},
			want:   []any{"abc", "https://x.com"},
		},
		{
			name:   "missing key binds nil preserving position",
			config: domain.JSONMap{"params": []any{"slug", "missing"}},
			input:  domain.JSONMap{"slug": "abc"},
			want:   []any{"abc", nil},
		},
		{
			name:    "non-array params",
			config:  domain.JSONMap{"params": "slug"},
			input:   domain.JSONMap{},
			wantErr: "params must be an array",
		},
		{
			name:    "non-string param name",
			config:  domain.JSONMap{"params": []any{"slug", 42}},
			input:   domain.JSONMap{},
			wantErr: "must be a string field name",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := dbBindArgs(tt.config, tt.input)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("got err %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("arg[%d] = %v, want %v", i, got[i], tt.want[i])
				}
			}
		})
	}
}
