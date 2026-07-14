// fleetctl — throwaway mTLS client to drive runtime.v1.FleetOrchestration on the
// KVM fleet host for the rt#9 durable-volume verification. NOT production code;
// keep untracked. Dials :50061 over mTLS with an SVID from the workload API and
// attaches the shared api-key as authz metadata.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffegrpc/grpccredentials"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	runtimev1 "github.com/sentiae/runtime-service/gen/proto/runtime/v1"
)

// stringList is a repeatable string flag (e.g. -secret-ref a -secret-ref b).
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	var secretRefs stringList
	flag.Var(&secretRefs, "secret-ref", "tenant-namespaced secret ref (repeatable): tenants/<org>/<subpath>#<field>")
	var (
		target   = flag.String("target", "127.0.0.1:50061", "fleet gRPC target")
		sock     = flag.String("sock", "unix:///run/spire/agent-sockets/api.sock", "SPIFFE workload API socket")
		apiKey   = flag.String("apikey", "", "shared service api key (authz metadata)")
		op       = flag.String("op", "provision", "provision|health|decommission|scale")
		handle   = flag.String("handle", "", "app handle (health/decommission/scale)")
		replicas = flag.Int("replicas", 1, "scale replicas")
		// provision descriptor fields
		component = flag.String("component", "volprobe", "component_id")
		env       = flag.String("env", "prod", "env")
		registry  = flag.String("registry", "10.0.10.20:8078", "OCI registry")
		repo      = flag.String("repo", "fleetprobe/volprobe", "OCI repository")
		digest    = flag.String("digest", "", "OCI digest sha256:...")
		port      = flag.Int("port", 8080, "guest port")
		ownerOrg  = flag.String("owner-org", "", "owner org uuid")
		volSizeMB = flag.Int("vol-mb", 128, "volume size MB (0 = no volume)")
		s2z       = flag.Bool("scale-to-zero", false, "mark app scale-to-zero eligible")
		idleTTL   = flag.Int("idle-ttl", 0, "idle_ttl_seconds (0 = never idle-out)")
	)
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	src, err := workloadapi.NewX509Source(ctx, workloadapi.WithClientOptions(workloadapi.WithAddr(*sock)))
	if err != nil {
		die("x509 source: %v", err)
	}
	defer src.Close()

	svid, err := src.GetX509SVID()
	if err != nil {
		die("get svid: %v", err)
	}
	fmt.Fprintf(os.Stderr, "client SVID: %s\n", svid.ID)

	creds := grpccredentials.MTLSClientCredentials(src, src, tlsconfig.AuthorizeAny())
	conn, err := grpc.NewClient(*target, grpc.WithTransportCredentials(creds))
	if err != nil {
		die("dial: %v", err)
	}
	defer conn.Close()

	if *apiKey != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-api-key", *apiKey)
	}
	cli := runtimev1.NewFleetOrchestrationClient(conn)

	switch *op {
	case "provision":
		desc := &runtimev1.DeploymentDescriptor{
			ComponentId: *component,
			Env:         *env,
			Image: &runtimev1.OCIImageRef{
				Registry:   *registry,
				Repository: *repo,
				Digest:     *digest,
			},
			Port:          int32(*port),
			WorkloadClass: "resident",
			ScaleToZero:   *s2z,
			IdleTtlSeconds: int32(*idleTTL),
			SecretRefs:    secretRefs,
		}
		if *volSizeMB > 0 {
			desc.Volumes = []*runtimev1.VolumeSpec{{Id: "", SizeMb: int32(*volSizeMB)}}
		}
		resp, err := cli.Provision(ctx, &runtimev1.ProvisionRequest{Descriptor_: desc, OwnerOrg: *ownerOrg})
		if err != nil {
			die("Provision: %v", err)
		}
		emit(map[string]any{"handle": resp.GetHandle(), "url": resp.GetUrl()})
	case "health":
		resp, err := cli.Health(ctx, &runtimev1.FleetHealthRequest{Handle: *handle})
		if err != nil {
			die("Health: %v", err)
		}
		emit(map[string]any{
			"state": resp.GetState(), "healthy": resp.GetHealthy(),
			"message": resp.GetMessage(), "url": resp.GetUrl(),
		})
	case "decommission":
		_, err := cli.Decommission(ctx, &runtimev1.FleetDecommissionRequest{Handle: *handle})
		if err != nil {
			die("Decommission: %v", err)
		}
		emit(map[string]any{"decommissioned": *handle})
	case "scale":
		_, err := cli.Scale(ctx, &runtimev1.FleetScaleRequest{Handle: *handle, Replicas: int32(*replicas)})
		if err != nil {
			die("Scale: %v", err)
		}
		emit(map[string]any{"scaled": *handle, "replicas": *replicas})
	default:
		die("unknown op %q", *op)
	}
}

func emit(v map[string]any) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}

func die(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "fleetctl: "+strings.TrimRight(f, "\n")+"\n", a...)
	os.Exit(1)
}
