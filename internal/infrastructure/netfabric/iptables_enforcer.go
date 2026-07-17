// Package netfabric is the P21 NetworkFabric enforcement adapter for the
// sentiae_fleet target class (CP4.5 §9 #5, D-164): it compiles fleet network
// policies into an iptables program on the Firecracker host.
//
// # Why this package is the SINGLE WRITER of the fleet's FORWARD program
//
// Before #5, two uncoordinated writers appended to one ordered chain: the image
// booter's ensureNAT (a cross-tenant DROP plus two blanket /16 ACCEPTs, each at
// FORWARD position 1, on the FIRST VM boot) and applyEgressList (a per-tap jump,
// also at position 1, on every job boot). Last writer wins, so ordering was a
// function of boot sequence — which is exactly how a job's egress_allow came to
// sit ABOVE the cross-tenant DROP and buy lateral reach to every other tenant's
// microVM on the host.
//
// Two uncoordinated writers to one ordered chain IS the bug. Re-asserting the
// order from a loop only narrows the window; it does not close it, and a known
// window on the very boot that installs the control is not definitive. So this
// package owns the whole FORWARD program: one owner, one deterministic pass, and
// a layout AssertPosture can prove whole.
//
// # The program
//
//	FORWARD (the first 5 rules, exactly, in this order):
//	  1  -j SNT-XVM                                  anchor: the inter-VM DECISION
//	  2  -j SNT-EGRESS                               anchor: per-tap external egress jumps
//	  3  -s 10.201/16 -d 10.201/16 -j DROP           the floor beneath SNT-XVM (belt and braces)
//	  4  -s 10.201/16 -j ACCEPT                      guest egress
//	  5  -d 10.201/16 -j ACCEPT                      ingress from host/Caddy
//
//	SNT-XVM:
//	  -s 10.201/16 -d 10.201/16 -j SNT-SYS-<h>       one per active system network
//	  ...
//	  -s 10.201/16 -d 10.201/16 -j DROP              THE TERMINAL DENY — always last
//
//	SNT-SYS-<h>:                                     one per system×env; rebuilt whole
//	  -s <srcGuest>/32 -d <dstGuest>/32 -p tcp --dport <port> -j ACCEPT
//
//	SNT-EGRESS:
//	  -i img<N> -j fc-eg-img<N>                      the existing per-tap chain, unchanged
//
// The anchors sit ABOVE both the floor DROP and the blanket ACCEPTs, which is
// what makes the design work at all: an inter-VM packet reaches SNT-XVM before
// the floor can swallow it (so a policy can ACCEPT), and an egress packet reaches
// SNT-EGRESS before rule 4 can ACCEPT it (so an allowlist's trailing DROP still
// governs). Get that order wrong and the control is either dead or silently open.
//
// Fail-closed by topology, not by vigilance: every path out of SNT-XVM other than
// an explicit ACCEPT inside a system chain ends at its terminal DROP, and
// SNT-EGRESS is only reachable for flows SNT-XVM returned — i.e. flows with at
// most one end in the fleet subnet. An egress allowlist therefore cannot name a
// fleet-internal peer into existence. The permissive branch is not disabled by
// config; it is unreachable.
package netfabric

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

const (
	// FleetSubnet is the /16 the image-boot path carves per-workload /30s out of —
	// the scope of every rule this package writes.
	FleetSubnet = domain.FleetSubnetCIDR

	// XVMChain is the inter-VM decision anchor. Terminal for any flow with both
	// ends in FleetSubnet.
	XVMChain = "SNT-XVM"

	// EgressParentChain is the parent the per-tap egress chains hang under. The
	// image-boot path passes it to applyEgressList so a job's allowlist lands
	// BELOW the inter-VM decision and can never re-open inter-VM reach.
	EgressParentChain = "SNT-EGRESS"

	// systemChainPrefix + 16 hex chars = 24 chars, inside iptables' 28-char chain
	// name limit (mirrors egressChainName's discipline).
	systemChainPrefix = "SNT-SYS-"
	systemChainHexLen = 16

	// maxRuleDeleteLoops bounds the "delete every copy" convergence loop so a
	// misbehaving iptables can never spin us forever.
	maxRuleDeleteLoops = 32
)

// Runner executes one iptables invocation and returns its combined output. It is
// injected so the enforcer is unit-testable with no root and no KVM.
type Runner func(ctx context.Context, args ...string) ([]byte, error)

// execRunner shells out to the real iptables binary — the same child-process
// pattern the rest of the fleet's networking uses (no cgo, no netlink bindings).
func execRunner(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "iptables", args...).CombinedOutput()
}

// IPTablesEnforcer realizes fleet network policy as an iptables program.
type IPTablesEnforcer struct {
	run Runner
}

var _ usecase.NetworkEnforcer = (*IPTablesEnforcer)(nil)

// NewIPTablesEnforcer constructs the enforcer over the real iptables binary.
func NewIPTablesEnforcer() *IPTablesEnforcer {
	return &IPTablesEnforcer{run: execRunner}
}

// NewIPTablesEnforcerWithRunner constructs the enforcer over an injected runner.
func NewIPTablesEnforcerWithRunner(r Runner) *IPTablesEnforcer {
	return &IPTablesEnforcer{run: r}
}

// forwardProgram is the intended FORWARD program, in order. It is the single
// source of truth for both InstallSkeleton (which writes it) and AssertPosture
// (which proves it) — the two can never drift apart.
func forwardProgram() [][]string {
	return [][]string{
		{"-j", XVMChain},
		{"-j", EgressParentChain},
		{"-s", FleetSubnet, "-d", FleetSubnet, "-j", "DROP"},
		{"-s", FleetSubnet, "-j", "ACCEPT"},
		{"-d", FleetSubnet, "-j", "ACCEPT"},
	}
}

// xvmTerminalRule is SNT-XVM's terminal deny. Always last, always present.
func xvmTerminalRule() []string {
	return []string{"-s", FleetSubnet, "-d", FleetSubnet, "-j", "DROP"}
}

// systemJumpRule is the SNT-XVM jump into one system's chain.
func systemJumpRule(chain string) []string {
	return []string{"-s", FleetSubnet, "-d", FleetSubnet, "-j", chain}
}

// SystemChainName returns the deterministic, length-bounded chain name for a
// network. Derived from the uuid so a restart recomputes the same name and
// converges onto the same chain rather than orphaning it.
func SystemChainName(networkID uuid.UUID) string {
	sum := sha256.Sum256(networkID[:])
	return systemChainPrefix + hex.EncodeToString(sum[:])[:systemChainHexLen]
}

// InstallSkeleton writes the complete FORWARD program plus the anchor chains,
// converging from ANY starting state: a fresh host, a post-reboot host, or a
// host still carrying the pre-#5 ensureNAT rules (the live upgrade path — kernel
// rules survive a process restart, so a clean slate must never be assumed).
//
// Convergence works by deleting every existing copy of each intended rule
// wherever it sits and then re-inserting the program at the top in reverse
// order. The pre-#5 ensureNAT rules are byte-identical to program rules 3-5, so
// the same delete pass adopts them rather than duplicating them.
func (e *IPTablesEnforcer) InstallSkeleton(ctx context.Context) error {
	for _, chain := range []string{XVMChain, EgressParentChain} {
		if err := e.ensureChain(ctx, chain); err != nil {
			return err
		}
	}

	// A per-tap egress jump left in FORWARD by a pre-#5 process would end up BELOW
	// program rule 4 (-s /16 -j ACCEPT) once the program is installed, which would
	// silently disable that job's allowlist — the exact fail-open this package
	// exists to remove. Reparent them into SNT-EGRESS instead of orphaning them.
	if err := e.reparentStaleEgressJumps(ctx); err != nil {
		return err
	}

	program := forwardProgram()
	for _, rule := range program {
		if err := e.deleteAllCopies(ctx, "FORWARD", rule); err != nil {
			return err
		}
	}
	// Insert in reverse so the program lands in order at positions 1..N.
	for i := len(program) - 1; i >= 0; i-- {
		args := append([]string{"-I", "FORWARD", "1"}, program[i]...)
		if out, err := e.run(ctx, args...); err != nil {
			return fmt.Errorf("install FORWARD program %v: %s: %w", program[i], strings.TrimSpace(string(out)), err)
		}
	}

	// The terminal deny must be SNT-XVM's LAST rule. Deleting every copy and
	// appending guarantees that without disturbing system jumps already installed
	// above it by a previous process.
	if err := e.deleteAllCopies(ctx, XVMChain, xvmTerminalRule()); err != nil {
		return err
	}
	args := append([]string{"-A", XVMChain}, xvmTerminalRule()...)
	if out, err := e.run(ctx, args...); err != nil {
		return fmt.Errorf("install %s terminal deny: %s: %w", XVMChain, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// AssertPosture PROVES the host's layout matches the intended program exactly —
// exact rules, exact order — rather than checking a pairwise precedence. A
// pairwise check is not enough: "the cross-tenant DROP precedes the egress jump"
// is TRUE of the broken layout where ensureNAT's rules sit above the anchors and
// the whole control is dead. Only the full program distinguishes the two.
//
// The program must be the FIRST rules of FORWARD. A foreign rule above them is a
// refusal, not a warning: we cannot prove it does not preempt us, and a control
// that cannot prove itself must prevent the operation (D-162a L3).
func (e *IPTablesEnforcer) AssertPosture(ctx context.Context) error {
	rules, err := e.listChain(ctx, "FORWARD")
	if err != nil {
		return err
	}
	program := forwardProgram()
	if len(rules) < len(program) {
		return fmt.Errorf("%w: FORWARD has %d rules, the program needs %d",
			domain.ErrNetworkPostureUnproven, len(rules), len(program))
	}
	for i, want := range program {
		if !equalRule(rules[i], want) {
			return fmt.Errorf("%w: FORWARD rule %d is %q, want %q",
				domain.ErrNetworkPostureUnproven, i+1, strings.Join(rules[i], " "), strings.Join(want, " "))
		}
	}

	xvm, err := e.listChain(ctx, XVMChain)
	if err != nil {
		return err
	}
	if len(xvm) == 0 {
		return fmt.Errorf("%w: %s is empty (no terminal deny)", domain.ErrNetworkPostureUnproven, XVMChain)
	}
	if last := xvm[len(xvm)-1]; !equalRule(last, xvmTerminalRule()) {
		return fmt.Errorf("%w: %s last rule is %q, want the terminal deny %q",
			domain.ErrNetworkPostureUnproven, XVMChain, strings.Join(last, " "), strings.Join(xvmTerminalRule(), " "))
	}
	if _, err := e.listChain(ctx, EgressParentChain); err != nil {
		return err
	}
	return nil
}

// SyncSystem atomically rebuilds ONE system's allow chain from the resolved
// rules: whole-chain flush + refill, never incremental, so it is idempotent and
// self-healing (a reboot's new guest IP converges on the next tick with no
// caller involvement).
func (e *IPTablesEnforcer) SyncSystem(ctx context.Context, networkID uuid.UUID, rules []usecase.ResolvedRule) error {
	// Validate the WHOLE batch before touching the host: a half-written chain is a
	// lie about enforcement.
	for _, r := range rules {
		if err := validateResolved(r); err != nil {
			return err
		}
	}

	chain := SystemChainName(networkID)
	if err := e.ensureChain(ctx, chain); err != nil {
		return err
	}
	if out, err := e.run(ctx, "-F", chain); err != nil {
		return fmt.Errorf("flush %s: %s: %w", chain, strings.TrimSpace(string(out)), err)
	}

	// From here the chain is EMPTY (denying). If the refill fails partway, leave it
	// empty rather than half-filled: guest IPs are allocated per boot and freed
	// indices are reused, so a stale ACCEPT can come to name a DIFFERENT tenant's
	// microVM. A failed sync must deny, not keep yesterday's addresses alive.
	var syncErr error
	defer func() {
		if syncErr != nil {
			if out, ferr := e.run(ctx, "-F", chain); ferr != nil {
				// iptables is broken; AssertPosture will refuse the next provision.
				_ = out
			}
		}
	}()

	for _, r := range rules {
		args := []string{
			"-A", chain,
			"-s", r.SrcIP + "/32",
			"-d", r.DstIP + "/32",
			"-p", r.Protocol,
			"--dport", strconv.Itoa(r.Port),
			"-j", "ACCEPT",
		}
		if out, err := e.run(ctx, args...); err != nil {
			syncErr = fmt.Errorf("add rule to %s: %s: %w", chain, strings.TrimSpace(string(out)), err)
			return syncErr
		}
	}

	// Link the chain ABOVE SNT-XVM's terminal deny. Delete-then-insert-at-1 keeps
	// the link exactly once and always above the DROP, from any starting state.
	jump := systemJumpRule(chain)
	if err := e.deleteAllCopies(ctx, XVMChain, jump); err != nil {
		syncErr = err
		return syncErr
	}
	args := append([]string{"-I", XVMChain, "1"}, jump...)
	if out, err := e.run(ctx, args...); err != nil {
		syncErr = fmt.Errorf("link %s into %s: %s: %w", chain, XVMChain, strings.TrimSpace(string(out)), err)
		return syncErr
	}
	return nil
}

// DropSystem unlinks and removes a system's chain entirely (Deprovision).
func (e *IPTablesEnforcer) DropSystem(ctx context.Context, networkID uuid.UUID) error {
	chain := SystemChainName(networkID)
	if err := e.deleteAllCopies(ctx, XVMChain, systemJumpRule(chain)); err != nil {
		return err
	}
	if out, err := e.run(ctx, "-F", chain); err != nil {
		return fmt.Errorf("flush %s: %s: %w", chain, strings.TrimSpace(string(out)), err)
	}
	if out, err := e.run(ctx, "-X", chain); err != nil {
		return fmt.Errorf("delete %s: %s: %w", chain, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// validateResolved is defense in depth behind the resolver's own guards: a rule
// that is not port-exact and confined to the fleet subnet never reaches the host.
func validateResolved(r usecase.ResolvedRule) error {
	if r.Protocol != domain.PolicyProtocolTCP {
		return fmt.Errorf("%w: %q", domain.ErrUnsupportedPolicyProtocol, r.Protocol)
	}
	if r.Port <= 0 || r.Port > 65535 {
		return fmt.Errorf("%w: port %d", domain.ErrInvalidNetworkPolicy, r.Port)
	}
	for _, ip := range []string{r.SrcIP, r.DstIP} {
		if !domain.InFleetSubnet(ip) {
			return fmt.Errorf("%w: %q is not a fleet guest address", domain.ErrInvalidNetworkPolicy, ip)
		}
	}
	return nil
}

// ensureChain creates a chain if absent. iptables -N fails when the chain already
// exists, so existence is confirmed by listing it rather than by -N's exit code.
func (e *IPTablesEnforcer) ensureChain(ctx context.Context, chain string) error {
	if _, err := e.run(ctx, "-N", chain); err == nil {
		return nil
	}
	if _, err := e.listChain(ctx, chain); err != nil {
		return fmt.Errorf("ensure chain %s: %w", chain, err)
	}
	return nil
}

// deleteAllCopies removes every copy of rule from chain, wherever it sits. It is
// how the install converges from an unknown starting state without assuming how
// many copies a previous writer left.
func (e *IPTablesEnforcer) deleteAllCopies(ctx context.Context, chain string, rule []string) error {
	for i := 0; i < maxRuleDeleteLoops; i++ {
		check := append([]string{"-C", chain}, rule...)
		if _, err := e.run(ctx, check...); err != nil {
			return nil // absent → converged
		}
		del := append([]string{"-D", chain}, rule...)
		if out, err := e.run(ctx, del...); err != nil {
			return fmt.Errorf("delete %v from %s: %s: %w", rule, chain, strings.TrimSpace(string(out)), err)
		}
	}
	return fmt.Errorf("delete %v from %s: still present after %d passes", rule, chain, maxRuleDeleteLoops)
}

// reparentStaleEgressJumps moves any per-tap egress jump a pre-#5 process left in
// FORWARD into SNT-EGRESS, preserving its restriction instead of letting the
// program's blanket ACCEPT silently preempt it.
func (e *IPTablesEnforcer) reparentStaleEgressJumps(ctx context.Context) error {
	rules, err := e.listChain(ctx, "FORWARD")
	if err != nil {
		return err
	}
	for _, rule := range rules {
		if !isEgressJump(rule) {
			continue
		}
		if err := e.deleteAllCopies(ctx, "FORWARD", rule); err != nil {
			return err
		}
		args := append([]string{"-A", EgressParentChain}, rule...)
		if out, err := e.run(ctx, args...); err != nil {
			return fmt.Errorf("reparent egress jump %v: %s: %w", rule, strings.TrimSpace(string(out)), err)
		}
	}
	return nil
}

// isEgressJump reports whether a FORWARD rule is a per-tap egress chain jump
// (`-i <tap> -j fc-eg-<tap>`; see firecracker.egressChainName).
func isEgressJump(rule []string) bool {
	for i := 0; i < len(rule)-1; i++ {
		if rule[i] == "-j" && strings.HasPrefix(rule[i+1], "fc-eg-") {
			return true
		}
	}
	return false
}

// listChain returns chain's rules in order, each as its argv tokens with the
// leading "-A <chain>" stripped. The chain policy line (-P) is not a rule and is
// dropped.
func (e *IPTablesEnforcer) listChain(ctx context.Context, chain string) ([][]string, error) {
	out, err := e.run(ctx, "-S", chain)
	if err != nil {
		return nil, fmt.Errorf("%w: list chain %s: %s: %w",
			domain.ErrNetworkPostureUnproven, chain, strings.TrimSpace(string(out)), err)
	}
	var rules [][]string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != "-A" || fields[1] != chain {
			continue
		}
		rules = append(rules, fields[2:])
	}
	return rules, nil
}

// equalRule compares two rules token-for-token.
func equalRule(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
