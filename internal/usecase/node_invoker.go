package usecase

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/sentiae/platform-kit/logger"
	"github.com/sentiae/platform-kit/nodeabi"
	"github.com/sentiae/platform-kit/nodebroker"
	"github.com/sentiae/runtime-service/internal/domain"
)

// proxyURL is the address a sandboxed bundle reaches its egress proxy on. The
// hostname is the sidecar's DNS alias on the invocation bridge, so it resolves
// to exactly one container and only inside that invocation's network.
const proxyURL = "http://proxy:3128"

// egressTokenBytes is the entropy behind one invocation's proxy bearer. It is
// rendered as lower hex and carries NO prefix: `handle:` belongs to secret
// handles alone, so a grep for it stays a secret-only signal (§9.6).
const egressTokenBytes = 32

// InvokeNodeInput is one node invocation: which bundle, with which resolved
// inputs and config, under which run and organization.
type InvokeNodeInput struct {
	RunID       uuid.UUID
	OrgID       uuid.UUID
	Environment string
	Node        *domain.GraphNode
	Inputs      map[string]json.RawMessage
	Config      map[string]json.RawMessage
	SecretToken string
}

// InvokeNodeOutput is what a completed invocation produced. Outputs are raw
// JSON because the runtime moves them between nodes without interpreting them
// — the ONE exception is a respond node's `response`, which the runtime does
// interpret and therefore validates here.
type InvokeNodeOutput struct {
	Outputs    map[string]json.RawMessage
	Fired      map[string]bool
	Logs       []nodeabi.LogEntry
	DurationMS int64
}

// NodeInvoker runs ONE node: it resolves the secrets the node declared, opens a
// sidecar when the node needs one, launches the bundle by digest in a sandbox,
// and turns the RESULT document into outputs or a failure.
//
// It is the only place a secret VALUE exists in this process, and the value
// never reaches the node: the node is handed a handle, the sidecar is handed
// the answer on an attached stdin stream, and the two meet over a unix socket
// the node mounts read-only.
type NodeInvoker struct {
	runner       BundleRunner
	sidecars     SidecarManager
	secrets      SecretValueSource
	pool         *SubnetPool
	registryHost string
	clock        Clock
}

// NewNodeInvoker wires the invoker to its ports.
func NewNodeInvoker(
	runner BundleRunner,
	sidecars SidecarManager,
	secrets SecretValueSource,
	pool *SubnetPool,
	registryHost string,
	clock Clock,
) *NodeInvoker {
	return &NodeInvoker{
		runner:       runner,
		sidecars:     sidecars,
		secrets:      secrets,
		pool:         pool,
		registryHost: registryHost,
		clock:        clock,
	}
}

// RevokeSecretToken hands the run's token back at terminal cleanup. It is on
// the invoker because the invoker owns the value source; the engine owns WHEN.
func (i *NodeInvoker) RevokeSecretToken(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return i.secrets.Revoke(ctx, token)
}

// Invoke runs one node to a terminal outcome. A returned error is the node's
// failure reason verbatim (§3.6.6) — the engine names the node around it.
func (i *NodeInvoker) Invoke(ctx context.Context, in InvokeNodeInput) (InvokeNodeOutput, error) {
	node := in.Node
	if node == nil || node.NodeRef == nil {
		return InvokeNodeOutput{}, domain.ErrNodeRefRequired
	}
	started := i.clock.Now()
	invocationID := "inv-" + uuid.NewString()
	image := i.imageRef(node.NodeRef)

	logger.FromContext(ctx).Info("node_invocation_started",
		"run", in.RunID.String(), "node", node.Name, "pin", node.NodeRef.Pin(),
		"digest", node.NodeRef.Digest, "secrets", len(node.Secrets), "egress", len(node.Egress))

	out, err := i.invoke(ctx, in, invocationID, image)
	duration := i.clock.Now().Sub(started).Milliseconds()
	out.DurationMS = duration

	status := "completed"
	outcome := outcomeOK
	if err != nil {
		status = "failed"
		outcome = outcomeError
	}
	usecaseExecutions.WithLabelValues("invoke_node", outcome).Inc()
	logger.FromContext(ctx).Info("node_invocation_finished",
		"run", in.RunID.String(), "node", node.Name, "status", status, "duration_ms", duration)
	return out, err
}

func (i *NodeInvoker) invoke(ctx context.Context, in InvokeNodeInput, invocationID, image string) (InvokeNodeOutput, error) {
	node := in.Node

	// Secrets are resolved BEFORE anything is launched: a node that cannot get
	// a secret it requires must never run at all, because a half-configured
	// bundle produces a plausible wrong answer rather than a refusal.
	handles, answers, err := i.resolveSecrets(ctx, in)
	if err != nil {
		return InvokeNodeOutput{}, err
	}

	var egress *nodeabi.Egress
	var egressToken string
	if node.NeedsBridge() {
		egressToken, err = newEgressToken()
		if err != nil {
			return InvokeNodeOutput{}, fmt.Errorf("mint egress token: %w", err)
		}
		egress = &nodeabi.Egress{Proxy: proxyURL, Token: egressToken}
	}

	call := nodeabi.Call{
		ABI:        nodeabi.ABI,
		Config:     orEmptyRaw(in.Config),
		Egress:     egress,
		Inputs:     orEmptyRaw(in.Inputs),
		Invocation: nodeabi.Invocation{ID: invocationID, Node: node.Name, RunID: in.RunID.String()},
		Node:       node.NodeRef.Pin(),
		Secrets:    handles,
	}
	callBytes, err := json.Marshal(call)
	if err != nil {
		return InvokeNodeOutput{}, fmt.Errorf("encode call: %w", err)
	}

	if err := i.runner.Pull(ctx, image); err != nil {
		return InvokeNodeOutput{}, err
	}

	// One sidecar per invocation that declares secrets OR egress; a bridge only
	// for egress. A node with neither gets nothing at all: no sidecar, no
	// network, no mount.
	var sc Sidecar
	if node.NeedsSidecar() {
		subnet := ""
		if node.NeedsBridge() {
			subnet, err = i.pool.Acquire()
			if err != nil {
				return InvokeNodeOutput{}, err
			}
			defer i.pool.Release(subnet)
		}
		binding := SidecarBinding{
			Invocation: invocationID,
			Run:        in.RunID.String(),
			Node:       node.Name,
			Secrets:    answers,
		}
		if node.NeedsBridge() {
			binding.Egress = &EgressBinding{
				Patterns: append([]string(nil), node.Egress...),
				Token:    egressToken,
				Subnet:   subnet,
			}
		}
		sc, err = i.sidecars.Open(ctx, SidecarOpen{
			InvocationID: invocationID,
			RunID:        in.RunID,
			Node:         node.Name,
			Binding:      binding,
		})
		if err != nil {
			return InvokeNodeOutput{}, err
		}
		// Teardown runs on every path including cancellation, so it gets a
		// context that outlives the one the wave cancels: a cancelled ctx here
		// would leak the sidecar and its bridge until the run sweep.
		defer func() {
			if cerr := i.sidecars.Close(context.WithoutCancel(ctx), invocationID); cerr != nil {
				logger.FromContext(ctx).Warn("sidecar_close_failed",
					"run", in.RunID.String(), "invocation_id", invocationID, "err", cerr)
			}
		}()
	}

	brokerSubpath := ""
	if len(node.Secrets) > 0 {
		brokerSubpath = sc.BrokerSubpath
	}

	res, err := i.runner.Run(ctx, BundleLaunch{
		RunID:         in.RunID,
		InvocationID:  invocationID,
		Image:         image,
		Call:          callBytes,
		MemoryMB:      node.Resources.MemoryMB,
		TimeoutSec:    node.Resources.TimeoutSec,
		BrokerSubpath: brokerSubpath,
		Network:       sc.Network,
	})
	if err != nil {
		return InvokeNodeOutput{}, err
	}
	return i.result(node, res)
}

// secretResolveUnavailablePhrase is the FIXED tail of a secret-resolution
// failure as the caller sees it. The resolver's own text is deliberately not
// carried: a Vault refusal names the mount, the path, the policy and the token's
// accessor, and this string lands verbatim on the node execution row and in the
// editor. The class (`secret_resolve_failed`) and the secret NAME are the parts
// a flow author can act on; the cause belongs to the operator, in the log.
const secretResolveUnavailablePhrase = "unavailable"

// secretResolveError fixes the TEXT a caller sees while keeping the CHAIN a
// caller in-process can inspect: Error() never renders the cause, Unwrap()
// still exposes it, so errors.Is/As keep working on the resolver's sentinels
// without the transport detail ever reaching a user-facing string.
type secretResolveError struct {
	name  string
	cause error
}

func (e *secretResolveError) Error() string {
	return "secret_resolve_failed: " + e.name + ": " + secretResolveUnavailablePhrase
}

func (e *secretResolveError) Unwrap() error { return e.cause }

// resolveSecrets turns the node's DECLARED secrets into handles for the node
// and answers for the sidecar. The value never enters the CALL, a log line, an
// argument or an environment variable.
func (i *NodeInvoker) resolveSecrets(ctx context.Context, in InvokeNodeInput) (map[string]string, map[string]SecretAnswer, error) {
	handles := map[string]string{}
	answers := map[string]SecretAnswer{}
	for _, spec := range in.Node.Secrets {
		value, found, err := i.secrets.Resolve(ctx, in.OrgID, in.SecretToken, in.Environment, spec.Name)
		if err != nil {
			// A resolver failure is a FAILURE — a 403 is never softened into
			// "not set", which would empty every optional secret and let the run
			// report success on a misconfigured tenant. Only the wording is
			// reduced; the full cause goes to the operator's log at ERROR.
			logger.FromContext(ctx).Error("secret_resolve_failed",
				"run", in.RunID.String(), "org", in.OrgID.String(),
				"environment", in.Environment, "secret", spec.Name, "err", err)
			return nil, nil, &secretResolveError{name: spec.Name, cause: err}
		}
		if spec.Required && !found {
			return nil, nil, fmt.Errorf("%w: %s", domain.ErrRequiredSecretAbsent, spec.Name)
		}
		handle := nodebroker.NewHandle()
		handles[spec.Name] = handle
		answers[spec.Name] = SecretAnswer{Handle: handle, Found: found, Value: value}
	}
	return handles, answers, nil
}

// result turns one sandbox run into outputs or the verbatim failure reason.
func (i *NodeInvoker) result(node *domain.GraphNode, res BundleRunResult) (InvokeNodeOutput, error) {
	if res.TimedOut {
		return InvokeNodeOutput{}, fmt.Errorf("crash: timeout after %ds", node.Resources.TimeoutSec)
	}
	if res.ExitCode != 0 {
		return InvokeNodeOutput{}, fmt.Errorf("crash: exit status %d: %s", res.ExitCode, lastLine(res.Stderr))
	}
	if len(res.Stdout) == 0 {
		return InvokeNodeOutput{}, errors.New("crash: no result document on stdout")
	}

	declared := make([]nodeabi.DeclaredOutput, 0, len(node.Ports.Outputs))
	for _, o := range node.Ports.Outputs {
		declared = append(declared, nodeabi.DeclaredOutput{Name: o.Name, Required: o.Required})
	}
	parsed, verr := nodeabi.ValidateResult(res.Stdout, declared)
	if verr != nil {
		return InvokeNodeOutput{}, fmt.Errorf("%s: %s", verr.Code, verr.Message)
	}
	if parsed.Status == nodeabi.StatusError {
		msg := fmt.Sprintf("%s: %s", parsed.Error.Code, parsed.Error.Message)
		if parsed.Error.Retryable {
			msg += " (retryable)"
		}
		return InvokeNodeOutput{Logs: parsed.Logs}, errors.New(msg)
	}

	fired := make(map[string]bool, len(parsed.Emitted))
	for _, name := range parsed.Emitted {
		fired[name] = true
	}
	// `response` is the ONE output the runtime itself interprets, so its shape
	// is the runtime's own contract and is checked here. Coercing a malformed
	// one would ship a broken HTTP response instead of naming the defect.
	if node.Role == domain.NodeRoleRespond && fired["response"] {
		if err := validateResponse(parsed.Outputs["response"]); err != nil {
			return InvokeNodeOutput{Logs: parsed.Logs}, err
		}
	}
	return InvokeNodeOutput{Outputs: parsed.Outputs, Fired: fired, Logs: parsed.Logs}, nil
}

// responseInvalidReason is the verbatim node failure for a respond node whose
// `response` output the runtime cannot answer a caller with.
const responseInvalidReason = "response_invalid: response must be an object with an integer status"

// validateResponse holds the shape §3.6.4 answers a caller with: an object, an
// integer status in 100–599, and — when present — object headers.
func validateResponse(raw json.RawMessage) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return errors.New(responseInvalidReason)
	}
	status, ok := obj["status"]
	if !ok {
		return errors.New(responseInvalidReason)
	}
	var code json.Number
	if err := json.Unmarshal(status, &code); err != nil {
		return errors.New(responseInvalidReason)
	}
	n, err := code.Int64()
	if err != nil || n < 100 || n > 599 {
		return errors.New(responseInvalidReason)
	}
	if headers, ok := obj["headers"]; ok {
		var h map[string]json.RawMessage
		if err := json.Unmarshal(headers, &h); err != nil {
			return errors.New(responseInvalidReason)
		}
	}
	return nil
}

// imageRef is what the sandbox pulls: the configured TLS registry, the
// repository the pin resolved to, and the digest. The tag the reference
// happens to carry is never used — a tag can be moved after a graph is built.
func (i *NodeInvoker) imageRef(ref *domain.NodeRef) string {
	repo := ref.Repository()
	if i.registryHost == "" {
		return repo + "@" + ref.Digest
	}
	return i.registryHost + "/" + repo + "@" + ref.Digest
}

func newEgressToken() (string, error) {
	buf := make([]byte, egressTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// orEmptyRaw keeps the CALL's required objects present: a nil map encodes as
// JSON null, and the ABI reads a null required field as MISSING.
func orEmptyRaw(in map[string]json.RawMessage) map[string]json.RawMessage {
	if in == nil {
		return map[string]json.RawMessage{}
	}
	return in
}

// lastLine is the tail of a crashed process's stderr — the line that usually
// names the fault, without shipping the whole stream into a database column.
func lastLine(stderr string) string {
	trimmed := strings.TrimRight(stderr, "\n")
	if trimmed == "" {
		return ""
	}
	if idx := strings.LastIndex(trimmed, "\n"); idx >= 0 {
		return trimmed[idx+1:]
	}
	return trimmed
}
