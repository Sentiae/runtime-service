//go:build unit

package netfabric

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// fakeIPT is an in-memory iptables: it answers -S/-C/-N/-D/-A/-I/-F/-X against a
// map of chains, so the enforcer's real argv path is exercised with no root and
// no KVM. It records every invocation for ordering assertions.
type fakeIPT struct {
	chains map[string][][]string
	calls  [][]string
	// failOn makes the runner return an error for the first invocation whose argv
	// contains this token — used to drive the fail paths.
	failOn string
}

func newFakeIPT() *fakeIPT {
	return &fakeIPT{chains: map[string][][]string{"FORWARD": {}}}
}

func (f *fakeIPT) run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	if f.failOn != "" {
		for _, a := range args {
			if a == f.failOn {
				return []byte("fake failure"), errors.New("fake iptables failure")
			}
		}
	}
	if len(args) == 0 {
		return nil, errors.New("no args")
	}
	switch args[0] {
	case "-N":
		if _, ok := f.chains[args[1]]; ok {
			return []byte("iptables: Chain already exists."), errors.New("exit 1")
		}
		f.chains[args[1]] = [][]string{}
		return nil, nil
	case "-S":
		rules, ok := f.chains[args[1]]
		if !ok {
			return []byte("No chain/target/match by that name."), errors.New("exit 1")
		}
		var b strings.Builder
		fmt.Fprintf(&b, "-N %s\n", args[1])
		for _, r := range rules {
			fmt.Fprintf(&b, "-A %s %s\n", args[1], strings.Join(r, " "))
		}
		return []byte(b.String()), nil
	case "-C":
		if f.indexOf(args[1], args[2:]) < 0 {
			return nil, errors.New("exit 1")
		}
		return nil, nil
	case "-A":
		f.chains[args[1]] = append(f.chains[args[1]], args[2:])
		return nil, nil
	case "-I":
		chain := args[1]
		rule := args[2:]
		pos := 1
		if len(args) > 2 && args[2] == "1" {
			rule = args[3:]
		}
		rules := f.chains[chain]
		idx := pos - 1
		out := make([][]string, 0, len(rules)+1)
		out = append(out, rules[:idx]...)
		out = append(out, rule)
		out = append(out, rules[idx:]...)
		f.chains[chain] = out
		return nil, nil
	case "-D":
		i := f.indexOf(args[1], args[2:])
		if i < 0 {
			return nil, errors.New("exit 1")
		}
		rules := f.chains[args[1]]
		f.chains[args[1]] = append(append([][]string{}, rules[:i]...), rules[i+1:]...)
		return nil, nil
	case "-F":
		if _, ok := f.chains[args[1]]; !ok {
			return nil, errors.New("exit 1")
		}
		f.chains[args[1]] = [][]string{}
		return nil, nil
	case "-X":
		delete(f.chains, args[1])
		return nil, nil
	}
	return nil, errors.New("unsupported: " + strings.Join(args, " "))
}

func (f *fakeIPT) indexOf(chain string, rule []string) int {
	for i, r := range f.chains[chain] {
		if equalRule(r, rule) {
			return i
		}
	}
	return -1
}

func (f *fakeIPT) ruleStrings(chain string) []string {
	out := make([]string, 0, len(f.chains[chain]))
	for _, r := range f.chains[chain] {
		out = append(out, strings.Join(r, " "))
	}
	return out
}

// posOf returns the index of the first rule in chain whose joined form contains
// substr, or -1.
func (f *fakeIPT) posOf(chain, substr string) int {
	for i, r := range f.ruleStrings(chain) {
		if strings.Contains(r, substr) {
			return i
		}
	}
	return -1
}

func newTestEnforcer() (*IPTablesEnforcer, *fakeIPT) {
	f := newFakeIPT()
	return NewIPTablesEnforcerWithRunner(f.run), f
}

func TestInstallSkeleton_WritesTheProgramInOrder(t *testing.T) {
	e, f := newTestEnforcer()
	if err := e.InstallSkeleton(context.Background()); err != nil {
		t.Fatalf("InstallSkeleton: %v", err)
	}

	want := []string{
		"-j " + XVMChain,
		"-j " + EgressParentChain,
		"-s " + FleetSubnet + " -d " + FleetSubnet + " -j DROP",
		"-s " + FleetSubnet + " -j ACCEPT",
		"-d " + FleetSubnet + " -j ACCEPT",
	}
	got := f.ruleStrings("FORWARD")
	if len(got) != len(want) {
		t.Fatalf("FORWARD has %d rules, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("FORWARD rule %d = %q, want %q", i+1, got[i], want[i])
		}
	}
}

// The two anchors must precede BOTH the floor DROP and the blanket ACCEPTs.
// If the DROP came first, every network policy would be unreachable; if the
// -s ACCEPT came first, every job's egress allowlist would be silently disabled.
func TestInstallSkeleton_AnchorsPrecedeDropAndAccepts(t *testing.T) {
	e, f := newTestEnforcer()
	if err := e.InstallSkeleton(context.Background()); err != nil {
		t.Fatalf("InstallSkeleton: %v", err)
	}
	xvm := f.posOf("FORWARD", "-j "+XVMChain)
	egress := f.posOf("FORWARD", "-j "+EgressParentChain)
	drop := f.posOf("FORWARD", "-j DROP")
	srcAccept := f.posOf("FORWARD", "-s "+FleetSubnet+" -j ACCEPT")

	if xvm < 0 || egress < 0 || drop < 0 || srcAccept < 0 {
		t.Fatalf("missing rule: xvm=%d egress=%d drop=%d srcAccept=%d", xvm, egress, drop, srcAccept)
	}
	if !(xvm < egress) {
		t.Fatalf("SNT-XVM jump (%d) must precede SNT-EGRESS jump (%d)", xvm, egress)
	}
	if !(xvm < drop) {
		t.Fatalf("SNT-XVM jump (%d) must precede the floor DROP (%d) or every policy is dead", xvm, drop)
	}
	if !(egress < srcAccept) {
		t.Fatalf("SNT-EGRESS jump (%d) must precede the blanket egress ACCEPT (%d) or every allowlist is a no-op", egress, srcAccept)
	}
}

func TestInstallSkeleton_TerminalDenyIsLastInXVM(t *testing.T) {
	e, f := newTestEnforcer()
	if err := e.InstallSkeleton(context.Background()); err != nil {
		t.Fatalf("InstallSkeleton: %v", err)
	}
	rules := f.ruleStrings(XVMChain)
	if len(rules) == 0 {
		t.Fatal("SNT-XVM is empty; want a terminal deny")
	}
	want := "-s " + FleetSubnet + " -d " + FleetSubnet + " -j DROP"
	if last := rules[len(rules)-1]; last != want {
		t.Fatalf("SNT-XVM last rule = %q, want %q", last, want)
	}
}

func TestInstallSkeleton_Idempotent(t *testing.T) {
	e, f := newTestEnforcer()
	ctx := context.Background()
	if err := e.InstallSkeleton(ctx); err != nil {
		t.Fatalf("first InstallSkeleton: %v", err)
	}
	first := f.ruleStrings("FORWARD")
	if err := e.InstallSkeleton(ctx); err != nil {
		t.Fatalf("second InstallSkeleton: %v", err)
	}
	second := f.ruleStrings("FORWARD")
	if strings.Join(first, "|") != strings.Join(second, "|") {
		t.Fatalf("not idempotent:\nfirst:  %v\nsecond: %v", first, second)
	}
	if err := e.AssertPosture(ctx); err != nil {
		t.Fatalf("AssertPosture after re-install: %v", err)
	}
}

// The live upgrade path: a host still carrying the pre-#5 ensureNAT rules. The
// install must ADOPT them into the program rather than duplicate them or leave
// them on top.
func TestInstallSkeleton_ConvergesFromPreExistingEnsureNATRules(t *testing.T) {
	e, f := newTestEnforcer()
	f.chains["FORWARD"] = [][]string{
		{"-s", FleetSubnet, "-d", FleetSubnet, "-j", "DROP"},
		{"-s", FleetSubnet, "-j", "ACCEPT"},
		{"-d", FleetSubnet, "-j", "ACCEPT"},
	}
	ctx := context.Background()
	if err := e.InstallSkeleton(ctx); err != nil {
		t.Fatalf("InstallSkeleton: %v", err)
	}
	if err := e.AssertPosture(ctx); err != nil {
		t.Fatalf("AssertPosture after upgrade-in-place: %v", err)
	}
	if got := len(f.chains["FORWARD"]); got != 5 {
		t.Fatalf("FORWARD has %d rules after adopting the pre-#5 rules, want 5: %v", got, f.ruleStrings("FORWARD"))
	}
}

// A per-tap egress jump left in FORWARD by a pre-#5 process must be reparented
// into SNT-EGRESS: left in FORWARD it would sit below the blanket ACCEPT and its
// allowlist would silently stop restricting anything.
func TestInstallSkeleton_ReparentsStaleEgressJump(t *testing.T) {
	e, f := newTestEnforcer()
	f.chains["FORWARD"] = [][]string{
		{"-i", "img7", "-j", "fc-eg-img7"},
		{"-s", FleetSubnet, "-d", FleetSubnet, "-j", "DROP"},
	}
	if err := e.InstallSkeleton(context.Background()); err != nil {
		t.Fatalf("InstallSkeleton: %v", err)
	}
	if f.posOf("FORWARD", "fc-eg-img7") >= 0 {
		t.Fatalf("stale egress jump left in FORWARD: %v", f.ruleStrings("FORWARD"))
	}
	if f.posOf(EgressParentChain, "fc-eg-img7") < 0 {
		t.Fatalf("stale egress jump not reparented into %s: %v", EgressParentChain, f.ruleStrings(EgressParentChain))
	}
}

func TestAssertPosture_PassesOnTheInstalledProgram(t *testing.T) {
	e, _ := newTestEnforcer()
	ctx := context.Background()
	if err := e.InstallSkeleton(ctx); err != nil {
		t.Fatalf("InstallSkeleton: %v", err)
	}
	if err := e.AssertPosture(ctx); err != nil {
		t.Fatalf("AssertPosture: %v", err)
	}
}

// THE regression that matters. This is the exact layout the pre-#5 ensureNAT
// produced on a fresh host: DI init installs the anchors, then ensureNAT's
// sync.Once inserts its DROP + ACCEPTs at position 1 on the first VM boot,
// pushing the anchors below them. Every policy is dead and every egress
// allowlist is a no-op — and a PAIRWISE check ("the cross-tenant DROP precedes
// the egress jump") PASSES on it, which is why posture must prove the whole
// program instead.
func TestAssertPosture_RefusesEnsureNATOrdering(t *testing.T) {
	e, f := newTestEnforcer()
	ctx := context.Background()
	if err := e.InstallSkeleton(ctx); err != nil {
		t.Fatalf("InstallSkeleton: %v", err)
	}

	// Replay ensureNAT's pre-#5 behavior against the installed program.
	f.chains["FORWARD"] = append([][]string{
		{"-s", FleetSubnet, "-d", FleetSubnet, "-j", "DROP"},
		{"-s", FleetSubnet, "-j", "ACCEPT"},
		{"-d", FleetSubnet, "-j", "ACCEPT"},
	}, f.chains["FORWARD"]...)

	// Sanity: the broken layout satisfies the pairwise check that was originally
	// proposed as the acceptance assertion. It must NOT satisfy AssertPosture.
	dropPos := f.posOf("FORWARD", "-j DROP")
	egressPos := f.posOf("FORWARD", "-j "+EgressParentChain)
	if !(dropPos < egressPos) {
		t.Fatalf("fixture is wrong: want the DROP (%d) above the egress jump (%d)", dropPos, egressPos)
	}

	err := e.AssertPosture(ctx)
	if err == nil {
		t.Fatal("AssertPosture accepted the broken ensureNAT ordering — the control is dead and posture certified it")
	}
	if !errors.Is(err, domain.ErrNetworkPostureUnproven) {
		t.Fatalf("AssertPosture error = %v, want ErrNetworkPostureUnproven", err)
	}
}

func TestAssertPosture_RefusesForeignRuleOnTop(t *testing.T) {
	e, f := newTestEnforcer()
	ctx := context.Background()
	if err := e.InstallSkeleton(ctx); err != nil {
		t.Fatalf("InstallSkeleton: %v", err)
	}
	f.chains["FORWARD"] = append([][]string{{"-j", "SOMETHING-ELSE"}}, f.chains["FORWARD"]...)
	if err := e.AssertPosture(ctx); !errors.Is(err, domain.ErrNetworkPostureUnproven) {
		t.Fatalf("AssertPosture = %v, want ErrNetworkPostureUnproven for a foreign rule above the program", err)
	}
}

func TestAssertPosture_RefusesWhenXVMTerminalDenyIsMissing(t *testing.T) {
	e, f := newTestEnforcer()
	ctx := context.Background()
	if err := e.InstallSkeleton(ctx); err != nil {
		t.Fatalf("InstallSkeleton: %v", err)
	}
	f.chains[XVMChain] = [][]string{}
	if err := e.AssertPosture(ctx); !errors.Is(err, domain.ErrNetworkPostureUnproven) {
		t.Fatalf("AssertPosture = %v, want ErrNetworkPostureUnproven for a missing terminal deny", err)
	}
}

func TestAssertPosture_RefusesWhenTerminalDenyIsNotLast(t *testing.T) {
	e, f := newTestEnforcer()
	ctx := context.Background()
	if err := e.InstallSkeleton(ctx); err != nil {
		t.Fatalf("InstallSkeleton: %v", err)
	}
	// An ACCEPT appended after the deny would be unreachable, but a deny that is
	// not last means someone else is writing our chain — refuse.
	f.chains[XVMChain] = append(f.chains[XVMChain], []string{"-j", "ACCEPT"})
	if err := e.AssertPosture(ctx); !errors.Is(err, domain.ErrNetworkPostureUnproven) {
		t.Fatalf("AssertPosture = %v, want ErrNetworkPostureUnproven when the deny is not last", err)
	}
}

func TestAssertPosture_RefusesWhenEgressParentChainIsMissing(t *testing.T) {
	e, f := newTestEnforcer()
	ctx := context.Background()
	if err := e.InstallSkeleton(ctx); err != nil {
		t.Fatalf("InstallSkeleton: %v", err)
	}
	delete(f.chains, EgressParentChain)
	if err := e.AssertPosture(ctx); !errors.Is(err, domain.ErrNetworkPostureUnproven) {
		t.Fatalf("AssertPosture = %v, want ErrNetworkPostureUnproven for a missing %s", err, EgressParentChain)
	}
}

func TestSyncSystem_InstallsRulesAndKeepsTheDenyLast(t *testing.T) {
	e, f := newTestEnforcer()
	ctx := context.Background()
	if err := e.InstallSkeleton(ctx); err != nil {
		t.Fatalf("InstallSkeleton: %v", err)
	}
	nid := uuid.New()
	rules := []usecase.ResolvedRule{
		{SrcIP: "10.201.0.6", DstIP: "10.201.0.10", Protocol: "tcp", Port: 8080},
		{SrcIP: "10.201.0.6", DstIP: "10.201.0.14", Protocol: "tcp", Port: 8080},
	}
	if err := e.SyncSystem(ctx, nid, rules); err != nil {
		t.Fatalf("SyncSystem: %v", err)
	}

	chain := SystemChainName(nid)
	got := f.ruleStrings(chain)
	want := []string{
		"-s 10.201.0.6/32 -d 10.201.0.10/32 -p tcp --dport 8080 -j ACCEPT",
		"-s 10.201.0.6/32 -d 10.201.0.14/32 -p tcp --dport 8080 -j ACCEPT",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("system chain = %v, want %v", got, want)
	}

	xvm := f.ruleStrings(XVMChain)
	if xvm[len(xvm)-1] != "-s "+FleetSubnet+" -d "+FleetSubnet+" -j DROP" {
		t.Fatalf("terminal deny is no longer last in %s: %v", XVMChain, xvm)
	}
	jump := f.posOf(XVMChain, chain)
	if jump < 0 || jump >= len(xvm)-1 {
		t.Fatalf("system jump at %d must sit above the terminal deny (last=%d)", jump, len(xvm)-1)
	}
	if err := e.AssertPosture(ctx); err != nil {
		t.Fatalf("AssertPosture after SyncSystem: %v", err)
	}
}

func TestSyncSystem_EmptyRulesFlushesButKeepsTheJump(t *testing.T) {
	e, f := newTestEnforcer()
	ctx := context.Background()
	if err := e.InstallSkeleton(ctx); err != nil {
		t.Fatalf("InstallSkeleton: %v", err)
	}
	nid := uuid.New()
	if err := e.SyncSystem(ctx, nid, []usecase.ResolvedRule{
		{SrcIP: "10.201.0.6", DstIP: "10.201.0.10", Protocol: "tcp", Port: 8080},
	}); err != nil {
		t.Fatalf("SyncSystem: %v", err)
	}
	// The edge is deleted and the complete (empty) set re-applied.
	if err := e.SyncSystem(ctx, nid, nil); err != nil {
		t.Fatalf("SyncSystem empty: %v", err)
	}
	chain := SystemChainName(nid)
	if got := len(f.chains[chain]); got != 0 {
		t.Fatalf("system chain has %d rules after an empty sync, want 0: %v", got, f.ruleStrings(chain))
	}
	if f.posOf(XVMChain, chain) < 0 {
		t.Fatal("system jump was dropped by an empty sync; the chain must stay linked (and deny)")
	}
}

func TestSyncSystem_IsFlushAndRefillNotIncremental(t *testing.T) {
	e, f := newTestEnforcer()
	ctx := context.Background()
	if err := e.InstallSkeleton(ctx); err != nil {
		t.Fatalf("InstallSkeleton: %v", err)
	}
	nid := uuid.New()
	rules := []usecase.ResolvedRule{{SrcIP: "10.201.0.6", DstIP: "10.201.0.10", Protocol: "tcp", Port: 8080}}
	if err := e.SyncSystem(ctx, nid, rules); err != nil {
		t.Fatalf("first: %v", err)
	}
	first := f.ruleStrings(SystemChainName(nid))
	if err := e.SyncSystem(ctx, nid, rules); err != nil {
		t.Fatalf("second: %v", err)
	}
	second := f.ruleStrings(SystemChainName(nid))
	if strings.Join(first, "|") != strings.Join(second, "|") {
		t.Fatalf("not idempotent:\nfirst:  %v\nsecond: %v", first, second)
	}
	xvmJumps := 0
	for _, r := range f.ruleStrings(XVMChain) {
		if strings.Contains(r, SystemChainName(nid)) {
			xvmJumps++
		}
	}
	if xvmJumps != 1 {
		t.Fatalf("system jump appears %d times in %s, want exactly 1", xvmJumps, XVMChain)
	}
}

// A replica reboot: the old IP must not survive anywhere in the chain.
func TestSyncSystem_ReplacesRebootedReplicaIP(t *testing.T) {
	e, f := newTestEnforcer()
	ctx := context.Background()
	if err := e.InstallSkeleton(ctx); err != nil {
		t.Fatalf("InstallSkeleton: %v", err)
	}
	nid := uuid.New()
	if err := e.SyncSystem(ctx, nid, []usecase.ResolvedRule{
		{SrcIP: "10.201.0.6", DstIP: "10.201.0.10", Protocol: "tcp", Port: 8080},
	}); err != nil {
		t.Fatalf("SyncSystem: %v", err)
	}
	if err := e.SyncSystem(ctx, nid, []usecase.ResolvedRule{
		{SrcIP: "10.201.0.6", DstIP: "10.201.0.18", Protocol: "tcp", Port: 8080},
	}); err != nil {
		t.Fatalf("SyncSystem after reboot: %v", err)
	}
	rules := strings.Join(f.ruleStrings(SystemChainName(nid)), "|")
	if strings.Contains(rules, "10.201.0.10") {
		t.Fatalf("the old guest IP survived the reboot sync: %s", rules)
	}
	if !strings.Contains(rules, "10.201.0.18") {
		t.Fatalf("the new guest IP is missing: %s", rules)
	}
}

func TestSyncSystem_RejectsUnderSpecifiedRules(t *testing.T) {
	tests := []struct {
		name    string
		rule    usecase.ResolvedRule
		wantErr error
	}{
		{
			name:    "port zero is never any",
			rule:    usecase.ResolvedRule{SrcIP: "10.201.0.6", DstIP: "10.201.0.10", Protocol: "tcp", Port: 0},
			wantErr: domain.ErrInvalidNetworkPolicy,
		},
		{
			name:    "empty protocol is never tcp",
			rule:    usecase.ResolvedRule{SrcIP: "10.201.0.6", DstIP: "10.201.0.10", Protocol: "", Port: 80},
			wantErr: domain.ErrUnsupportedPolicyProtocol,
		},
		{
			name:    "src outside the fleet subnet",
			rule:    usecase.ResolvedRule{SrcIP: "10.0.10.20", DstIP: "10.201.0.10", Protocol: "tcp", Port: 80},
			wantErr: domain.ErrInvalidNetworkPolicy,
		},
		{
			name:    "dst outside the fleet subnet",
			rule:    usecase.ResolvedRule{SrcIP: "10.201.0.6", DstIP: "0.0.0.0", Protocol: "tcp", Port: 80},
			wantErr: domain.ErrInvalidNetworkPolicy,
		},
		{
			name:    "empty src",
			rule:    usecase.ResolvedRule{SrcIP: "", DstIP: "10.201.0.10", Protocol: "tcp", Port: 80},
			wantErr: domain.ErrInvalidNetworkPolicy,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, f := newTestEnforcer()
			ctx := context.Background()
			if err := e.InstallSkeleton(ctx); err != nil {
				t.Fatalf("InstallSkeleton: %v", err)
			}
			nid := uuid.New()
			err := e.SyncSystem(ctx, nid, []usecase.ResolvedRule{tt.rule})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("SyncSystem = %v, want %v", err, tt.wantErr)
			}
			// The whole batch is refused before the host is touched.
			if len(f.chains[SystemChainName(nid)]) != 0 {
				t.Fatalf("an invalid rule reached the host: %v", f.ruleStrings(SystemChainName(nid)))
			}
		})
	}
}

// A refill that fails partway must leave the chain EMPTY, not half-filled with
// yesterday's addresses: freed /30 indices are reused, so a stale ACCEPT can come
// to name a different tenant's microVM.
func TestSyncSystem_FailedRefillLeavesTheChainDenying(t *testing.T) {
	e, f := newTestEnforcer()
	ctx := context.Background()
	if err := e.InstallSkeleton(ctx); err != nil {
		t.Fatalf("InstallSkeleton: %v", err)
	}
	nid := uuid.New()
	if err := e.SyncSystem(ctx, nid, []usecase.ResolvedRule{
		{SrcIP: "10.201.0.6", DstIP: "10.201.0.10", Protocol: "tcp", Port: 8080},
	}); err != nil {
		t.Fatalf("SyncSystem: %v", err)
	}
	f.failOn = "--dport"
	err := e.SyncSystem(ctx, nid, []usecase.ResolvedRule{
		{SrcIP: "10.201.0.6", DstIP: "10.201.0.22", Protocol: "tcp", Port: 8080},
	})
	if err == nil {
		t.Fatal("SyncSystem succeeded despite a failing runner")
	}
	f.failOn = ""
	if got := f.ruleStrings(SystemChainName(nid)); len(got) != 0 {
		t.Fatalf("chain kept rules after a failed sync: %v — a failed sync must deny", got)
	}
}

func TestDropSystem_UnlinksAndRemovesTheChain(t *testing.T) {
	e, f := newTestEnforcer()
	ctx := context.Background()
	if err := e.InstallSkeleton(ctx); err != nil {
		t.Fatalf("InstallSkeleton: %v", err)
	}
	nid := uuid.New()
	if err := e.SyncSystem(ctx, nid, []usecase.ResolvedRule{
		{SrcIP: "10.201.0.6", DstIP: "10.201.0.10", Protocol: "tcp", Port: 8080},
	}); err != nil {
		t.Fatalf("SyncSystem: %v", err)
	}
	if err := e.DropSystem(ctx, nid); err != nil {
		t.Fatalf("DropSystem: %v", err)
	}
	chain := SystemChainName(nid)
	if _, ok := f.chains[chain]; ok {
		t.Fatalf("chain %s still exists", chain)
	}
	if f.posOf(XVMChain, chain) >= 0 {
		t.Fatalf("jump to %s still linked in %s: %v", chain, XVMChain, f.ruleStrings(XVMChain))
	}
	if err := e.AssertPosture(ctx); err != nil {
		t.Fatalf("AssertPosture after DropSystem: %v", err)
	}
}

func TestSystemChainName_IsBoundedAndDeterministic(t *testing.T) {
	nid := uuid.New()
	name := SystemChainName(nid)
	if len(name) > 28 {
		t.Fatalf("chain name %q is %d chars, iptables allows 28", name, len(name))
	}
	if name != SystemChainName(nid) {
		t.Fatal("chain name is not deterministic; a restart would orphan the chain")
	}
	if SystemChainName(uuid.New()) == name {
		t.Fatal("two networks collided on one chain name")
	}
}

func TestSystemChains_AreDisjointAcrossSystems(t *testing.T) {
	e, f := newTestEnforcer()
	ctx := context.Background()
	if err := e.InstallSkeleton(ctx); err != nil {
		t.Fatalf("InstallSkeleton: %v", err)
	}
	s, tt := uuid.New(), uuid.New()
	if err := e.SyncSystem(ctx, s, []usecase.ResolvedRule{
		{SrcIP: "10.201.0.6", DstIP: "10.201.0.10", Protocol: "tcp", Port: 8080},
	}); err != nil {
		t.Fatalf("sync S: %v", err)
	}
	if err := e.SyncSystem(ctx, tt, []usecase.ResolvedRule{
		{SrcIP: "10.201.0.18", DstIP: "10.201.0.22", Protocol: "tcp", Port: 8080},
	}); err != nil {
		t.Fatalf("sync T: %v", err)
	}
	sRules := strings.Join(f.ruleStrings(SystemChainName(s)), "|")
	if strings.Contains(sRules, "10.201.0.18") || strings.Contains(sRules, "10.201.0.22") {
		t.Fatalf("system S's chain contains system T's addresses: %s", sRules)
	}
}
