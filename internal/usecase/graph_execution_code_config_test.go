//go:build unit

package usecase

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/sentiae/runtime-service/internal/domain"
)

// fakeWarmRunner stands in for the Firecracker warm-clone executor (no Docker /
// KVM needed): it records the (language, code, stdin) it was handed — so the test
// can assert the F1 config seam (config.code / config.language) reached the
// executor — and simulates the JS runtime contract by reading `n` from the stdin
// JSON and emitting {"doubled": n*2} on stdout, exactly as the user's
// `console.log(JSON.stringify({doubled: input.n*2}))` body would.
type fakeWarmRunner struct {
	mu       sync.Mutex
	gotLang  domain.Language
	gotCode  string
	gotStdin string
}

func (f *fakeWarmRunner) RunCode(_ context.Context, language domain.Language, code, stdin string) (WarmRunResult, error) {
	f.mu.Lock()
	f.gotLang, f.gotCode, f.gotStdin = language, code, stdin
	f.mu.Unlock()

	// Simulate executing the user's JS body against stdin JSON.
	var in struct {
		N float64 `json:"n"`
	}
	_ = json.Unmarshal([]byte(stdin), &in)
	out, _ := json.Marshal(map[string]any{"doubled": in.N * 2})
	return WarmRunResult{Stdout: string(out), ExitCode: 0}, nil
}

// TestExecuteCodeNode_RunsUserCodeFromConfig proves a Code node executes the
// PER-INSTANCE user source carried in its config (the F1 seam: config = {language,
// code}) — NOT the dedicated Code/Language fields, which are left empty here — and
// that its output flows through shapeCodeOutput into the canonical node output.
// The sandboxed VM run itself is faked (no container/KVM in this env); what is
// proven is the dispatch + config-plumbing + output-shaping, end to end.
func TestExecuteCodeNode_RunsUserCodeFromConfig(t *testing.T) {
	const userCode = "let raw = '';\n" +
		"process.stdin.on('data', (chunk) => { raw += chunk; });\n" +
		"process.stdin.on('end', () => {\n" +
		"  const input = raw ? JSON.parse(raw) : {};\n" +
		"  console.log(JSON.stringify({ doubled: input.n * 2 }));\n" +
		"});\n"

	// Code node carrying its source + language ONLY in config (dedicated
	// Code/Language fields intentionally empty) to prove the config seam is read.
	codeNode := domain.GraphNode{
		ID:       uuid.New(),
		GraphID:  uuid.New(),
		NodeType: domain.GraphNodeTypeCode,
		Name:     "doubler",
		Config: domain.JSONMap{
			"language": "javascript",
			"code":     userCode,
		},
		SortOrder: 0,
	}

	// Sanity: the resolver must surface the config values (and the node must
	// validate on those alone, so a config-only code node deploys).
	if got := codeNode.ResolvedCode(); got != userCode {
		t.Fatalf("ResolvedCode did not read config.code:\n got %q", got)
	}
	if l := codeNode.ResolvedLanguage(); l == nil || *l != domain.LanguageJavaScript {
		t.Fatalf("ResolvedLanguage did not read config.language: got %v", l)
	}
	if err := codeNode.Validate(); err != nil {
		t.Fatalf("config-only code node failed validation: %v", err)
	}

	warm := &fakeWarmRunner{}
	eng := &GraphExecutionEngine{
		graphExecRepo:  memGraphExecRepo{},
		nodeExecRepo:   newMemNodeExecRepo(),
		executionUC:    newRecordingExecutionUC(),
		eventPublisher: noopEventPublisher{},
		warm:           warm,
		maxParallelism: 1,
		cancellations:  make(map[uuid.UUID]context.CancelFunc),
		timings:        make(map[uuid.UUID][]nodeTimings),
	}

	graphExec := &domain.GraphExecution{
		ID:             uuid.New(),
		GraphID:        uuid.New(),
		OrganizationID: uuid.New(),
		RequestedBy:    uuid.New(),
		Status:         domain.GraphExecPending,
		TotalNodes:     1,
	}
	graphDef := &domain.GraphDefinition{ID: graphExec.GraphID, Status: domain.GraphStatusActive}

	// Drive the engine with graph input {"n": 21}; the code node doubles it.
	input := domain.JSONMap{"n": float64(21)}
	eng.runGraph(context.Background(), graphExec, graphDef, []domain.GraphNode{codeNode}, nil, input, nil)

	// 1. The executor received the USER's code + language from config (F1 seam).
	warm.mu.Lock()
	gotLang, gotCode, gotStdin := warm.gotLang, warm.gotCode, warm.gotStdin
	warm.mu.Unlock()

	if gotLang != domain.LanguageJavaScript {
		t.Errorf("executor language: got %q, want javascript", gotLang)
	}
	if gotCode != userCode {
		t.Errorf("executor did NOT receive the user's config.code:\n got %q\nwant %q", gotCode, userCode)
	}

	// 2. The graph input reached the code node as stdin JSON.
	var stdin map[string]any
	if err := json.Unmarshal([]byte(gotStdin), &stdin); err != nil {
		t.Fatalf("stdin not JSON: %q (%v)", gotStdin, err)
	}
	if stdin["n"] != float64(21) {
		t.Errorf("stdin n: got %v, want 21 (stdin=%q)", stdin["n"], gotStdin)
	}

	// 3. The node output carries the contract shape {doubled: 42}, merged from the
	//    runtime's stdout JSON by shapeCodeOutput.
	rec, ok := eng.nodeExecRepo.(*memNodeExecRepo).byNodeName("doubler")
	if !ok {
		t.Fatal("no node-execution record written for the code node")
	}
	if rec.Status != domain.GraphExecCompleted {
		t.Fatalf("code node status: got %v, want completed (error=%q)", rec.Status, rec.Error)
	}
	if rec.Output == nil {
		t.Fatal("code node output is nil")
	}
	if got := rec.Output["doubled"]; got != float64(42) {
		t.Errorf("node output doubled: got %v, want 42 (output=%v)", got, rec.Output)
	}
	// The canonical base fields from shapeCodeOutput are present too.
	if !strings.Contains(toString(rec.Output["stdout"]), `"doubled":42`) {
		t.Errorf("node output stdout missing the run's JSON: %v", rec.Output["stdout"])
	}
}

func toString(v any) string {
	s, _ := v.(string)
	return s
}
