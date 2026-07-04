//go:build unit

package firecracker

import (
	"encoding/json"
	"testing"
)

func TestAgentRunRequestJSON(t *testing.T) {
	// Field names must match the guest-agent's runRequest exactly.
	req := AgentRunRequest{
		Language:  "python",
		Code:      "print(1)",
		Stdin:     "in",
		TimeoutMS: 5000,
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"language":"python","code":"print(1)","stdin":"in","timeout_ms":5000}`
	if string(b) != want {
		t.Fatalf("AgentRunRequest JSON:\n got %s\nwant %s", string(b), want)
	}

	// Round-trip back from the agent's wire shape.
	var got AgentRunRequest
	if err := json.Unmarshal([]byte(want), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != req {
		t.Fatalf("round-trip mismatch: got %#v want %#v", got, req)
	}
}

func TestAgentRunResultJSON(t *testing.T) {
	// Decode a wire payload exactly as the guest-agent emits it.
	wire := `{"stdout":"hi\n","stderr":"","exit_code":0,"duration_ms":8}`
	var res AgentRunResult
	if err := json.Unmarshal([]byte(wire), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if res.Stdout != "hi\n" || res.ExitCode != 0 || res.DurationMS != 8 {
		t.Fatalf("decoded wrong: %#v", res)
	}
	if res.Error != "" {
		t.Fatalf("Error should be empty, got %q", res.Error)
	}

	// Error is omitempty on the wire.
	b, _ := json.Marshal(AgentRunResult{Stdout: "x", ExitCode: 1})
	if string(b) != `{"stdout":"x","stderr":"","exit_code":1,"duration_ms":0}` {
		t.Fatalf("omitempty error broke: %s", string(b))
	}

	// And present when set.
	b, _ = json.Marshal(AgentRunResult{ExitCode: -1, Error: "timeout after 30000ms"})
	if string(b) != `{"stdout":"","stderr":"","exit_code":-1,"duration_ms":0,"error":"timeout after 30000ms"}` {
		t.Fatalf("error field wrong: %s", string(b))
	}
}
