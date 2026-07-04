# runtime-service — runtime-fleet: the microVM execution engine

**Pillar:** `docs/program/02-pillars/runtime-fleet/` · **Constitution:** root `CLAUDE.md` (binding) · **Topology:** `docs/program/01-architecture/topology.md`

The microVM engine — the thing that actually boots and runs a workload. It is a **worker of `delivery`** (delivery orchestrates, runtime-fleet executes; delivery reaches it only through the `DeployTarget` P7 port). Two roles over one proven substrate (warm-pool / snapshot / CoW): **(a)** the ephemeral Firecracker **test/preview sandbox** delivery uses to run a built image, and **(b)** the **Sentiae fleet** — the fly.io-like resident-hosting target (scheduler, host registry, resident VMs, reconciler, ingress/TLS, volumes, secrets, scale-to-zero). It **must NOT own**: the build→test→deploy→release saga / deploy policy (→ `delivery`), the OCI **image build** (→ delivery P6 — runtime *consumes* the image, never builds it), the system model / targets-as-data (→ `catalog` P3/P9), the autoscale **decision** (→ augur — the fleet only *actuates* `Scale` within owner-approved bounds, I7), or OCI **image storage** (→ vcs OCI-on-CAS, D-016). Read the pillar spec first; §4/§5 there are frozen.

---

## Directory layout — the real tree (⚠ PRE-CONSTITUTION, deviates from §3)

This service predates the constitution and does **not** use the canonical hexagonal tree. Match what exists when editing; do not rewrite wholesale (§32/§37). The real deltas from canonical: handlers are `internal/handler/` (not `internal/adapter/handler/`), repos are `internal/repository/postgres/` (not `internal/adapter/repository/`), there is **no `internal/port/` and no `internal/adapter/` tree**, ports/interfaces live in `internal/usecase/interfaces.go` + `internal/repository/interfaces.go`, and adapters live directly under `internal/infrastructure/`.

```
runtime-service/
├── cmd/
│   ├── server/main.go              # bootstrap (uses log.Printf — pre-platform-kit)
│   └── guest-agent/main.go         # static in-guest agent baked into the warm rootfs (python/node only)
├── internal/
│   ├── domain/                     # ~30 files: execution, microvm, snapshot, graph_* (interpreter),
│   │                               #   test_run, vm_instance, deployment_target, hermetic_build,
│   │                               #   compile, network_policy, runtime_agent, vm_usage …
│   ├── handler/                    # NOT adapter/handler
│   │   ├── grpc/                   # execution_server, graph_server, compile_server, test_run_server, server
│   │   ├── http/                   # ~25 handlers incl fleet_handler.go (per-host warm-pool introspection)
│   │   └── event/                  # continuous_test_trigger, post_merge_full_run
│   ├── usecase/                    # ~60 files
│   │   ├── graph_execution_engine.go   # the INTERPRETER (preview/debug only — I1); in-process http/db/transform nodes
│   │   ├── scheduler.go            # ⚠ single-localhost in-memory bin-packer STUB (not a fleet scheduler)
│   │   ├── vm_router.go vm_service.go vm_instance_service.go   # customer-VM routing (§9.3/9.4)
│   │   ├── compile_project.go warm_code_runner.go checkpoint_scheduler.go
│   │   ├── interfaces.go           # usecase-layer port interfaces (incl SchedulerUseCase)
│   │   └── … test dispatchers, coverage, quarantine, regression gen
│   ├── repository/
│   │   ├── interfaces.go
│   │   └── postgres/               # ~20 repos + migrations.go  ← ⚠ GORM AutoMigrate, NO migrations/ dir
│   ├── infrastructure/             # adapters live here
│   │   ├── firecracker/            # ✅ PRODUCTION-GRADE, single-host: warm.go warm_pool.go pool.go
│   │   │                           #   provider.go (Run = rootfs injection), checkpoint_scheduler.go,
│   │   │                           #   agent_client.go, step_runner.go, vmcomm/ (vsock/tap transport)
│   │   ├── firecracker_customer/   # customer-hosted FC client (§9.4) — wire under DELIVERY P7, not here
│   │   ├── container/  simulated/  # non-KVM providers (container = homelab fallback; simulated = fakes)
│   │   ├── agent/ vmagent/         # remote/customer agent HTTP clients
│   │   ├── compiler/               # docker_compiler.go (the P5 Compile backend — `docker run --rm`)
│   │   ├── executors/{a11y,contract,perf,visual}/   # test-type executors (axe/pact/k6/playwright)
│   │   ├── messaging/              # Kafka consumers (session_commit_added, canvas_executed) + publisher
│   │   ├── objectstore/            # S3/MinIO caching store (durable warm-template persistence)
│   │   └── foundry/ gitservice/ canvasservice/ testdb/
│   ├── di/container.go             # single Container (1198 lines); provider chosen by executor_type
│   └── (no internal/app — lifecycle is in cmd/server/main.go)
├── pkg/{config,logger}
├── proto/runtime/v1/{runtime.proto, graph.proto}
├── scripts/build-warm-rootfs.sh   Dockerfile   go.mod    ← ⚠ NO Makefile, NO README
```

## Dependency direction

Intent is still inward (`handler → usecase → domain`; adapters under `infrastructure/` implement `usecase`/`repository` interfaces), but the folder names differ from §3. Keep new code pointing inward and reach infrastructure only through the interfaces in `usecase/interfaces.go` / `repository/interfaces.go`. Do not deepen the deviation — when you add a genuinely new seam, prefer the canonical name.

## How to add an RPC / provider / consumer

- **New RPC:** edit `proto/runtime/v1/{runtime,graph}.proto` → `buf generate` → handler in `internal/handler/grpc/` → usecase → wire in `di/container.go`. The **net-new `FleetOrchestration`** gRPC (`Provision/Health/Scale/Cutover/Decommission` + `RegisterHost/Heartbeat`) is the seam delivery's P7 `fleet` adapter binds to (pillar §4/§9.12) — a NEW shared contract, coordinate before freezing.
- **New DB state:** MUST introduce a `migrations/` dir with **golang-migrate** paired up/down (`add-migration` skill) — do NOT extend `repository/postgres/migrations.go` (`AutoMigrate`). The durable **fleet control plane** (host registry, resident-replica desired/actual, routes, volumes, secret bindings) is the first real migration set (I8: no console-only state).
- **New execution provider:** implement the `VMProvider`/`ExecutionRunner` interfaces under `internal/infrastructure/<provider>/`; select it in `di/container.go` by `executor_type`.
- **New inbound consumer:** `internal/infrastructure/messaging/<topic>_consumer.go`, thin → calls a usecase, idempotent.
- **Verify before done:** `deploy` + `deploy-verify` skills (homelab; executor defaults to the container provider — no KVM).

## Ports & events

**Provides (PROVIDER):** `DeployTarget` **P7** for the `fleet` (sentiae_fleet) and `test` classes — **net-new**; delivery's saga is the consumer (`Provision/Health/Scale/Cutover/Decommission`). `CompileVerifier` **P5** (sandboxed variant) — the existing `Compile` RPC; codegen consumes.
**Consumes (CONSUMER):** `ImageBuilder` **P6** output — the OCI `ImageRef`; runtime **boots the image** (OCI→ext4), never calls `BuildImage`. `NodeRegistry` **P1** — the preview interpreter reads `NodeDefinition.runtimeBinding` from node-service. `TelemetrySink` **P8** — resident apps + guest agent tail OTLP. `SecretResolver` **P14** — inject `secret_refs` into the VM at boot (net-new; no secrets path exists today).

**RPC surface (frozen):** `RuntimeService` (`Execute`/`ExecuteAsync`, `GetExecution{Status,Result}`, `CreateExecution`, `DispatchTestRun`, `GetTestCoverage`/`Delta`, `GetVMUsage`, **`Compile`**); `GraphService` (`CreateGraph`, `DeployGraph`, `ExecuteGraph`, `Get/List/Cancel GraphExecution`, `List/GetNodeExecution` — the interpreter, **preview/debug ONLY**, I1). Per-host HTTP (retained): `GET /fleet`, `DELETE /fleet/clones/{id}`, `POST /fleet/templates/{lang}/refresh` — what delivery's `Fleet` RPC aggregates.

**Events:** **Consumes** `sentiae.work.session.commit_added` (drives test triggers) + canvas-executed. **Produces NONE on the build chain** — it is a synchronous worker behind P7/P5 (health/capacity federate via P8/pulse, not a new topic). It does **NOT** subscribe `operate.autoscale.decided` — the autoscale decision arrives via delivery actuating `P7.Scale` (delivery is the actuator boundary, I11).

## Service-specific rules & pitfalls (from the code)

1. **`scheduler.go` is a single-localhost in-memory STUB — not a fleet scheduler.** `NewScheduler` registers `localhost` (`127.0.0.1`) and best-fit bin-packs a `map[string]*HostInfo` under a mutex — **no persistence, no heartbeat expiry, no reconciler, no resident mode, no real multi-host placement**. It is tied to the `vm_instance`/`vm_router` customer-VM model (§9.3/9.4), not the fleet. Rebuild it durable + multi-host (pillar §9.4/§9.5) with a real registry + reconciler; do not extend the stub.
2. **The microVM `Run` path INJECTS SOURCE into a per-language rootfs — it does NOT boot a compiled image (the I1 change).** `infrastructure/firecracker/provider.go` `Run()`: copies `<lang>.ext4`, loop-mounts the copy, `injectCode` writes the user's code + a run script, overwrites `/init`, boots, powers off, remounts to read stdout/stderr/exit. The `test` (and every deploy) path MUST become **boot the compiled OCI image (OCI→ext4)** and run its own entrypoint. `cmd/guest-agent` (the warm resident path) only knows python/node (`langCommand`) — it becomes irrelevant once the VM boots the image's entrypoint (language is an image concern).
3. **The interpreter runs http/database/transform/condition nodes IN-PROCESS — SSRF, zero isolation.** `usecase/graph_execution_engine.go` `executeHTTPNode` calls `e.httpClient.Do(req)` on an arbitrary node-config URL straight from the runtime process (shared 30s client, no allowlist); `executeDatabaseNode`/`executeTransformNode`/`executeConditionNode` are the same. Only `code` nodes are sandboxed (via the warm runner). Keep the interpreter **preview/debug ONLY and network-guarded** (I1) — never a deploy path; in the compiled model these become real code inside the VM-isolated image.
4. **Warm-pool / CoW / firecracker substrate is production-grade but SINGLE-HOST and mostly EPHEMERAL — reuse it, don't rebuild it.** `firecracker/warm.go` + `warm_pool.go` + `pool.go`: CoW clones off a template snapshot (~160ms), per-VM `/30` netns+veth+DNAT isolation, virtio-rng, jailer (chroot+seccomp+cgroup), MinIO/object-store template persistence with cross-host pull + a background replenisher — all real, all on ONE host, unit-testable without KVM via the `warmManager` fake. But state is **AutoMigrated + ephemeral**. The resident fleet (host registry, reconciler, ingress/TLS/custom-domains, volumes, secrets injection, durable desired/actual, scale-to-zero with warm-resume) is **net-new on top of this substrate** — layer onto the primitives, don't reimplement them.
5. **Constitution deviations already present — match, but don't propagate.** The gRPC server (`handler/grpc/server.go`) uses `log.Printf` + hand-rolled logging/recovery/**dev-auth** interceptors + a LOCAL `userIDKey` context key — NOT platform-kit logger/interceptors/typed-key (§3/§15/§18/§23). Persistence is `AutoMigrate` (no `migrations/`). No Makefile/README. `DeploymentTarget{sentiae_hosted|customer_hosted|agent}` + `vm_router.go` + `firecracker_customer/` are runtime's *local* customer-VM routing notion — **distinct from** catalog's target-as-data (P3); the customer-firecracker path wires under **delivery's** P7 `customer_firecracker` adapter, not orphaned here. Config-drift note: config default gRPC port is `50062` but `.env.service` (deployed) sets **50061** — the value delivery dials.

## External dependencies

- **PostgreSQL 16** (GORM, AutoMigrate today) — execution/graph/test/vm state.
- **Firecracker** + **KVM** (`/dev/kvm`) where available; **container provider** fallback on the homelab (no KVM — `executor_type=container`, the default); **simulated** provider fakes runs.
- **Docker** — the `Compile` (P5) backend (`docker run --rm` toolchain images) and the container provider.
- **MinIO / S3** (`objectstore/`) — durable warm-template persistence + cross-host pull.
- **Kafka** (`kafka:9094`) — inbound consumers (session-commit, canvas-executed).
- **node-service** (P1), **foundry/git/canvas** gRPC clients (`infrastructure/{foundry,gitservice,canvasservice}`).
- Later (runtime-fleet): **Vault/KMS** (P14 secrets injection), **vcs OCI-on-CAS** (D-016, image pull), an **ingress/gateway + TLS** owner (open decision).

## Ports (frozen) & build commands

Ports: HTTP **8090** (fleet introspection, execution, graph debug, `/health`), gRPC **50061** (`RuntimeService` + `GraphService`), metrics port per config. Firecracker paths under `APP_FIRECRACKER_*`.

No Makefile — build/test directly:
```
buf generate                                        # regenerate proto stubs
go build ./cmd/... ./internal/...                   # NOT ./... (test files pull unrelated deps)
go test -tags unit ./...                            # warm-pool unit-tests via the warmManager fake (no KVM)
go test -tags integration ./...                     # testcontainers / KVM host
```

Deploy via `scripts/deploy.sh -- runtime-service` (see the `deploy` skill). Build the warm rootfs with `scripts/build-warm-rootfs.sh`.
