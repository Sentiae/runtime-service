package domain

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// EgressVerdict is the proxy's answer: the closed vocabulary the audit keeps.
type EgressVerdict string

const (
	EgressAllow EgressVerdict = "allow"
	EgressDeny  EgressVerdict = "deny"
)

// maxHostLen is the DNS name bound; host is tenant-chosen and this is its only
// size limit.
const maxHostLen = 253

// RedactedEgressHost is the host string the sidecar substitutes when the name a
// node asked for carries one of that invocation's own secrets. It is spelled
// here because the table's CHECK ties HostRedacted to exactly this value.
const RedactedEgressHost = "[redacted]"

// EgressDecision is one aggregated audit fact: within one invocation the
// sidecar's proxy answered (Decision, Reason) for (Host, Port) Hits times. The
// per-request lines are the sidecar's and die with it; this is what the
// platform keeps.
type EgressDecision struct {
	RunID        uuid.UUID
	InvocationID string
	Node         string
	Decision     EgressVerdict
	Reason       string
	Host         string
	Port         int
	Hits         int64
	FirstAt      time.Time
	LastAt       time.Time
	// Capped is true when the sidecar stopped writing decision lines at its
	// per-invocation cap: Hits is then a floor, not a total.
	Capped bool
	// HostRedacted is true exactly when Host is RedactedEgressHost: the name was
	// refused because it carried a bound secret.
	HostRedacted bool
}

// Validate refuses a decision the table would refuse, before the round trip.
// Hits is not checked: it is accumulated after construction.
func (d EgressDecision) Validate() error {
	switch {
	case d.RunID == uuid.Nil:
		return errors.New("run id is required")
	case d.InvocationID == "":
		return errors.New("invocation id is required")
	case d.Node == "":
		return errors.New("node is required")
	case d.Decision != EgressAllow && d.Decision != EgressDeny:
		return errors.New("decision must be allow or deny")
	case d.Reason == "":
		return errors.New("reason is required")
	case len(d.Host) == 0 || len(d.Host) > maxHostLen:
		return errors.New("host length must be 1..253")
	case d.HostRedacted != (d.Host == RedactedEgressHost):
		return errors.New("host redacted flag must match the redacted host")
	case d.Port < 1 || d.Port > 65535:
		return errors.New("port must be 1..65535")
	}
	return nil
}
