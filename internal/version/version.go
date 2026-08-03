// Package version carries the deploy-authoritative source provenance stamped
// into this binary at image build time.
//
// The values come from the Dockerfile's required VCS_REVISION / VCS_MODIFIED
// build arguments, which deploy.sh derives from the staged committed tree it is
// about to ship. The SAME arguments set the final image's
// org.opencontainers.image.revision label, so the binary's report and the image
// label match by construction — provenance cannot drift between them.
//
// A locally built binary (plain `go build`) leaves these at their zero values.
// That is not a silent gap: the Dockerfile refuses to build an image without a
// valid 40-hex revision, so no deployed image can report an empty revision.
package version

// Injected with -ldflags -X at image build time. Strings, not typed values,
// because -X can only set a string variable.
var (
	// Revision is the full 40-hex commit of the service repository the image
	// was built from.
	Revision = ""

	// Modified reports whether the shipped tree deviated from that commit:
	// "false" for a selective deploy (which ships committed HEAD by
	// construction), "true" when a full deploy shipped a dirty working tree.
	Modified = ""
)
