package config

import (
	"fmt"
	"time"

	pkconfig "github.com/sentiae/platform-kit/config"
)

// Config represents the complete application configuration.
type Config struct {
	App           AppConfig           `mapstructure:"app"`
	Logging       LoggingConfig       `mapstructure:"logging"`
	Server        ServerConfig        `mapstructure:"server"`
	Database      DatabaseConfig      `mapstructure:"database"`
	Services      ServicesConfig      `mapstructure:"services"`
	Firecracker   FirecrackerConfig   `mapstructure:"firecracker"`
	Container     ContainerConfig     `mapstructure:"container"`
	Kafka         KafkaConfig         `mapstructure:"kafka"`
	Hermetic      HermeticConfig      `mapstructure:"hermetic"`
	SnapshotStore SnapshotStoreConfig `mapstructure:"snapshot_store"`
	Registry      RegistryConfig      `mapstructure:"registry"`
	ImageBoot     ImageBootConfig     `mapstructure:"imageboot"`
	Fleet         FleetConfig         `mapstructure:"fleet"`
	Telemetry     TelemetryConfig     `mapstructure:"telemetry"`
	Resource      ResourceConfig      `mapstructure:"resource"`
}

// ResourceConfig configures the P19 durable resource control plane (CP4.5 §9 #3,
// D-183): the dedicated data-VM engine image, the shared logical-database engine
// endpoint, and the shared-tier reclamation TTL + seed-template allowlist.
type ResourceConfig struct {
	// EnginePGImageRegistry/Repository/Digest name the compiled Postgres engine
	// image the dedicated tier boots as a resident data-VM. All three are required
	// for a dedicated provision (an empty ref fails the underlying image-boot).
	EnginePGImageRegistry   string `mapstructure:"engine_pg_image_registry"`
	EnginePGImageRepository string `mapstructure:"engine_pg_image_repository"`
	EnginePGImageDigest     string `mapstructure:"engine_pg_image_digest"`
	// ConnBudget is the advertised connection budget reported in a dedicated
	// resource's status (informational; defaults to 100).
	ConnBudget int `mapstructure:"conn_budget"`
	// SharedPGHost/SharedPGPort address the SEPARATE Postgres engine on the fleet
	// host that backs the shared tier's logical databases (NOT the control-plane
	// DB — D-027/D-103). Published as a shared resource's endpoint.
	SharedPGHost string `mapstructure:"shared_pg_host"`
	SharedPGPort int    `mapstructure:"shared_pg_port"`
	// SharedTTL is the lifetime of a shared logical database before the reaper
	// reclaims it (expires_at = now()+SharedTTL).
	SharedTTL time.Duration `mapstructure:"shared_ttl"`
	// SharedSeedTemplates is the operator-controlled allowlist of template
	// databases a shared logical database may be cloned from. A requested seed
	// outside this set is rejected (never raw caller input).
	SharedSeedTemplates []string `mapstructure:"shared_seed_templates"`
}

// TelemetryConfig configures the OTLP export path (traces/metrics/logs → the
// otel-collector). Both fields are defaulted (never validate:required) so the
// service boots without telemetry env set; init is non-fatal (D-179 Wave-8).
type TelemetryConfig struct {
	ServiceName  string `mapstructure:"service_name"`
	OTLPEndpoint string `mapstructure:"otlp_endpoint"`
}

// FleetConfig configures this runtime-service instance's self-registration as a
// fleet host + its heartbeat loop (runtime-fleet CP4 §9#4). Only active when the
// firecracker executor is selected (a compose instance cannot boot images).
type FleetConfig struct {
	// HostID pins this host's fleet id. Empty ⇒ a stable UUIDv5 is derived from
	// the advertise host so restarts re-register the same row.
	HostID string `mapstructure:"host_id"`
	// Region is the placement region label reported to the registry.
	Region string `mapstructure:"region"`
	// HostDiskMB is the advertised disk capacity for image rootfs staging.
	HostDiskMB int64 `mapstructure:"host_disk_mb"`
	// HeartbeatInterval is how often the self-host heartbeats the registry.
	HeartbeatInterval time.Duration `mapstructure:"heartbeat_interval"`
	// SecretSelfTest gates the host->guest vsock self-test (Phase 3.3): when set,
	// a secret-ref-less provision injects a NON-SECRET marker over the vsock
	// secret channel so the I32 mechanism is verifiable end-to-end. Off by
	// default; orthogonal to the real ErrSecretsNotSupported gate.
	SecretSelfTest bool `mapstructure:"secret_selftest"`
	// VolumeDir is the host root under which per-volume ext4 backing files are
	// materialized (rt#9). Empty ⇒ derived as <Firecracker.SnapshotPath>/volumes.
	VolumeDir string `mapstructure:"volume_dir"`
	// IngressDomain is the base domain for platform-issued app hostnames (rt#8,
	// D-079): a resident app is reachable at <slug>-<env>.<IngressDomain>.
	IngressDomain string `mapstructure:"ingress_domain"`
	// Caddy configures the co-located ingress gateway the fleet drives over its
	// loopback admin API (rt#8). Only used on the firecracker host.
	Caddy CaddyConfig `mapstructure:"caddy"`
	// ActivateTimeout bounds the scale-to-zero wake path (rt#11): how long the
	// activator blocks waiting for a woken replica to become resident + healthy
	// before returning a retryable 503. Must stay under Caddy's upstream timeout.
	ActivateTimeout time.Duration `mapstructure:"activate_timeout"`
	// ActivatorEndpoint is the loopback "host:port" Caddy reverse-proxies a
	// zero-upstream (scaled-to-zero) route to (rt#11). Matches the runtime HTTP
	// listener; only used on the firecracker host.
	ActivatorEndpoint string `mapstructure:"activator_endpoint"`
}

// CaddyConfig configures the embedded Caddy ingress gateway the fleet reconciler
// pushes the full route set to each tick (rt#8, D-079).
type CaddyConfig struct {
	// AdminEndpoint is the Caddy admin JSON API base URL (loopback-bound on the
	// fleet host). The syncer POSTs the full config to <AdminEndpoint>/load.
	AdminEndpoint string `mapstructure:"admin_endpoint"`
	// AccessLogPath is the host file the fleet Caddy server writes JSON access
	// logs to (#fleet-scale-to-zero-activity-feed, D-122). The activity feed
	// tails it to detect apps served directly through Caddy (bypassing the
	// activator) so SweepIdle does not scale a busy app to zero. Empty disables
	// access logging (and the feed); the syncer then emits no logging block.
	AccessLogPath string `mapstructure:"access_log_path"`
}

// RegistryConfig configures the OCI registry the image-boot path pulls compiled
// images from (vcs OCI-on-CAS, D-016). Plain HTTP on the homelab.
type RegistryConfig struct {
	Host       string `mapstructure:"host"`
	ServiceKey string `mapstructure:"service_key"`
}

// ImageBootConfig configures the OCI→ext4 image-boot path (runtime-fleet CP3).
type ImageBootConfig struct {
	// WorkDir is the root under which per-workload staging dirs + rootfs images
	// are materialized.
	WorkDir string `mapstructure:"work_dir"`
	// InitPath is the host path to the prebuilt image-init binary copied into
	// each rootfs as /sentiae/init.
	InitPath string `mapstructure:"init_path"`
	// HostPortMin/Max bound the host-port range published for resident workloads.
	HostPortMin int `mapstructure:"host_port_min"`
	HostPortMax int `mapstructure:"host_port_max"`
	// AdvertiseHost is the host advertised in resident URLs.
	AdvertiseHost string `mapstructure:"advertise_host"`
}

// SnapshotStoreConfig configures the durable object-store backing for
// Firecracker memory snapshots. When Enabled, the snapshot service
// uploads mem/state files to an S3/MinIO bucket (source of truth) and
// fronts it with a local FilesystemStore cache (fast restore path). When
// disabled the snapshot service stays on the original local-only flow.
type SnapshotStoreConfig struct {
	Enabled   bool   `mapstructure:"enabled"`
	Endpoint  string `mapstructure:"endpoint"`
	Bucket    string `mapstructure:"bucket"`
	AccessKey string `mapstructure:"access_key"`
	SecretKey string `mapstructure:"secret_key"`
	Region    string `mapstructure:"region"`
	UseSSL    bool   `mapstructure:"use_ssl"`
	PathStyle bool   `mapstructure:"path_style"`
	// CacheDir is the local FilesystemStore root used as the warm cache in
	// front of the durable bucket.
	CacheDir string `mapstructure:"cache_dir"`
}

// AppConfig contains application metadata.
type AppConfig struct {
	Name         string `mapstructure:"name"`
	Version      string `mapstructure:"version"`
	Environment  string `mapstructure:"environment"`
	ExecutorType string `mapstructure:"executor_type"` // "firecracker" or "container"
	UseVsock     bool   `mapstructure:"use_vsock"`     // Use vsock agent for Firecracker
}

// LoggingConfig contains logging configuration.
type LoggingConfig struct {
	Level  string `mapstructure:"level"`
	Format string `mapstructure:"format"`
	Output string `mapstructure:"output"`
}

// ServerConfig contains server configuration.
type ServerConfig struct {
	HTTP HTTPConfig `mapstructure:"http"`
	GRPC GRPCConfig `mapstructure:"grpc"`
}

// HTTPConfig contains HTTP server configuration.
type HTTPConfig struct {
	Enabled  bool           `mapstructure:"enabled"`
	Host     string         `mapstructure:"host"`
	Port     string         `mapstructure:"port"`
	BasePath string         `mapstructure:"base_path"`
	Timeouts TimeoutsConfig `mapstructure:"timeouts"`
}

// TimeoutsConfig contains timeout settings.
type TimeoutsConfig struct {
	Read     time.Duration `mapstructure:"read"`
	Write    time.Duration `mapstructure:"write"`
	Idle     time.Duration `mapstructure:"idle"`
	Shutdown time.Duration `mapstructure:"shutdown"`
}

// GRPCConfig contains gRPC server configuration.
type GRPCConfig struct {
	Enabled bool   `mapstructure:"enabled"`
	Host    string `mapstructure:"host"`
	Port    string `mapstructure:"port"`

	// ServiceAPIKey is the shared service-to-service token presented as the
	// x-api-key header by internal callers (e.g. deployment-service's Fleet
	// RPC hitting the /fleet control endpoints). Empty ⇒ in-cluster traffic is
	// trusted (dev parity); non-empty ⇒ a constant-time match is required.
	ServiceAPIKey string `mapstructure:"service_api_key"`

	// JWKSURL is the JWKS endpoint backing the gRPC user-token (RS256)
	// validator (identity-service). Consumed by the platform-kit tenant auth
	// interceptor; an invalid/unreachable value degrades to api-key-only auth
	// rather than failing boot.
	JWKSURL string `mapstructure:"jwks_url"`

	// JWTIssuer is the expected `iss` claim on inbound user tokens validated
	// by the JWKS-backed validator above.
	JWTIssuer string `mapstructure:"jwt_issuer"`
}

// DatabaseConfig contains database configuration.
type DatabaseConfig struct {
	Postgres PostgresConfig `mapstructure:"postgres"`
}

// PostgresConfig contains PostgreSQL configuration.
type PostgresConfig struct {
	Host       string           `mapstructure:"host"`
	Port       string           `mapstructure:"port"`
	User       string           `mapstructure:"user"`
	Password   string           `mapstructure:"password"`
	Database   string           `mapstructure:"database"`
	SSLMode    string           `mapstructure:"ssl_mode"`
	Pool       PoolConfig       `mapstructure:"pool"`
	Migrations MigrationsConfig `mapstructure:"migrations"`
	LogLevel   string           `mapstructure:"log_level"`
}

// PoolConfig contains connection pool settings.
type PoolConfig struct {
	MaxOpenConns int           `mapstructure:"max_open_conns"`
	MaxIdleConns int           `mapstructure:"max_idle_conns"`
	MaxLifetime  time.Duration `mapstructure:"max_lifetime"`
	MaxIdleTime  time.Duration `mapstructure:"max_idle_time"`
}

// MigrationsConfig contains migration settings.
type MigrationsConfig struct {
	Enabled bool   `mapstructure:"enabled"`
	Path    string `mapstructure:"path"`
}

// ServicesConfig contains external service endpoints.
type ServicesConfig struct {
	Identity   ServiceEndpoint `mapstructure:"identity"`
	Permission ServiceEndpoint `mapstructure:"permission"`
	Canvas     ServiceEndpoint `mapstructure:"canvas"`
	Git        ServiceEndpoint `mapstructure:"git"`
}

// ServiceEndpoint represents an external service configuration.
type ServiceEndpoint struct {
	Enabled bool          `mapstructure:"enabled"`
	URL     string        `mapstructure:"url"`
	Timeout time.Duration `mapstructure:"timeout"`
}

// FirecrackerConfig contains Firecracker microVM configuration.
type FirecrackerConfig struct {
	BinaryPath     string        `mapstructure:"binary_path"`
	JailerPath     string        `mapstructure:"jailer_path"`
	KernelPath     string        `mapstructure:"kernel_path"`
	RootfsBasePath string        `mapstructure:"rootfs_base_path"`
	SnapshotPath   string        `mapstructure:"snapshot_path"`
	SocketDir      string        `mapstructure:"socket_dir"`
	DefaultVCPU    int           `mapstructure:"default_vcpu"`
	DefaultMemMB   int           `mapstructure:"default_mem_mb"`
	MaxVCPU        int           `mapstructure:"max_vcpu"`
	MaxMemMB       int           `mapstructure:"max_mem_mb"`
	DefaultTimeout time.Duration `mapstructure:"default_timeout"`
	MaxTimeout     time.Duration `mapstructure:"max_timeout"`
	PoolSize       int           `mapstructure:"pool_size"`
	UseJailer      bool          `mapstructure:"use_jailer"`

	// ChrootBase is the jailer chroot base for image-boot microVMs: each VM is
	// confined to <chroot_base>/firecracker/<vm-id>/root.
	ChrootBase string `mapstructure:"chroot_base"`

	// VMUIDBase is the first uid handed to a jailed VMM (the per-VM uid is
	// VMUIDBase + the workload's network index). 100000 sits above Debian's
	// 0–64999 user range and systemd DynamicUser's 61184–65519, so the ids
	// collide with no host account and need no /etc/passwd entries.
	VMUIDBase int `mapstructure:"vm_uid_base"`

	// VMUIDSpan bounds the per-VM uid range. An index outside it refuses the boot
	// rather than silently handing a VM another tenant's uid.
	VMUIDSpan int `mapstructure:"vm_uid_span"`

	// §9.1 — pool size per language profile. Separate from PoolSize
	// which is the legacy single-bucket default. When 0, falls back
	// to PoolSize; when both are 0, the pool defaults to 2.
	PoolSizePerProfile int `mapstructure:"pool_size_per_profile"`

	// §9.1 — language profiles to pre-warm. Empty list disables the
	// pool entirely (cold boots only). Each language becomes its own
	// VMPool bucket with PoolSizePerProfile slots.
	PoolLanguages []string `mapstructure:"pool_languages"`

	// §9.3 — per-VM auto-snapshot interval (minutes). 0 falls back to
	// the package default (DefaultCheckpointIntervalMinutes = 15).
	CheckpointIntervalMinutes int `mapstructure:"checkpoint_interval_minutes"`

	// §9.3 — set false to skip wiring the per-VM auto-snapshot
	// scheduler. Defaults true in production-like environments; local
	// dev can set false to avoid writing snapshot files under
	// SnapshotPath.
	EnableCheckpointScheduler bool `mapstructure:"enable_checkpoint_scheduler"`

	// WarmPoolEnabled gates the warm-VM / snapshot-clone code-execution
	// path. When true (and executor_type=firecracker), code nodes run on
	// a fast warm clone (~160ms) instead of a single-shot cold boot.
	// Default false → cold path, zero behavior change.
	WarmPoolEnabled bool `mapstructure:"warm_pool_enabled"`

	// WarmPoolReady is the per-language pre-warm buffer depth: how many
	// already-restored clones the pool keeps ready so an execution grabs one
	// instantly (~0ms) instead of restoring on-demand (~160ms). 0 (default) ⇒
	// on-demand only — exact current behavior, no background goroutines. Capped
	// at maxWarmPoolReady so a misconfig can't exhaust the host.
	WarmPoolReady int `mapstructure:"warm_pool_ready"`
}

// maxWarmPoolReady caps WarmPoolReady so a fat-fingered value can't pre-warm
// enough clones to exhaust the host's index space / memory.
const maxWarmPoolReady = 32

// ContainerConfig contains Docker container executor configuration.
type ContainerConfig struct {
	DefaultVCPU    int           `mapstructure:"default_vcpu"`
	DefaultMemMB   int           `mapstructure:"default_mem_mb"`
	MaxVCPU        int           `mapstructure:"max_vcpu"`
	MaxMemMB       int           `mapstructure:"max_mem_mb"`
	DefaultTimeout time.Duration `mapstructure:"default_timeout"`
	MaxTimeout     time.Duration `mapstructure:"max_timeout"`

	// AllowHostNetwork gates whether an untrusted code-execution container
	// may use the host network namespace. Default false → every container
	// runs with --network none regardless of the requested NetworkMode.
	// Hostile code on the host network can reach internal services
	// (postgres, kafka, other pods) and exfiltrate secrets, so host
	// networking is an explicit, deliberate operator opt-in only.
	AllowHostNetwork bool `mapstructure:"allow_host_network"`
}

// HermeticConfig toggles §9.2 enforcement knobs. Both default off so
// existing callers (CI pipelines that haven't plumbed
// base_image_digest yet) don't break; operators opt in per environment.
type HermeticConfig struct {
	EnforceBaseImageDigest bool `mapstructure:"enforce_base_image_digest"`
	EnforceReproducibility bool `mapstructure:"enforce_reproducibility"`
}

// KafkaConfig contains Kafka event publishing configuration.
type KafkaConfig struct {
	Enabled bool     `mapstructure:"enabled"`
	Brokers []string `mapstructure:"brokers"`
	Topic   string   `mapstructure:"topic"`
	GroupID string   `mapstructure:"group_id"`
	// InboundTopics lists topics the runtime-service consumes from. Defaults
	// to the canvas execution-request topic when unset.
	InboundTopics []string `mapstructure:"inbound_topics"`
}

// Load loads configuration from config files, environment variables, and defaults.
// Environment variables use the APP_ prefix (e.g. APP_SERVER_HTTP_PORT).
func Load() (*Config, error) {
	var cfg Config
	err := pkconfig.Load(&cfg, pkconfig.Options{
		EnvPrefix:   "APP",
		ConfigPaths: []string{"configs", "."},
		Defaults: map[string]any{
			// App defaults
			"app.name":          "runtime-service",
			"app.version":       "dev",
			"app.environment":   "development",
			"app.executor_type": "container",

			// Logging defaults
			"logging.level":  "info",
			"logging.format": "json",
			"logging.output": "stdout",

			// HTTP server defaults
			"server.http.enabled":           true,
			"server.http.host":              "0.0.0.0",
			"server.http.port":              "8090",
			"server.http.base_path":         "/api/v1",
			"server.http.timeouts.read":     "30s",
			"server.http.timeouts.write":    "120s",
			"server.http.timeouts.idle":     "60s",
			"server.http.timeouts.shutdown": "30s",

			// gRPC server defaults
			"server.grpc.enabled":         true,
			"server.grpc.host":            "0.0.0.0",
			"server.grpc.port":            "50062",
			"server.grpc.service_api_key": "",
			"server.grpc.jwks_url":        "http://identity-service:8080/.well-known/jwks.json",
			"server.grpc.jwt_issuer":      "identity-service",

			// Database defaults
			"database.postgres.host":                "localhost",
			"database.postgres.port":                "5432",
			"database.postgres.user":                "postgres",
			"database.postgres.password":            "postgres",
			"database.postgres.database":            "runtime_service",
			"database.postgres.ssl_mode":            "disable",
			"database.postgres.pool.max_open_conns": 25,
			"database.postgres.pool.max_idle_conns": 10,
			"database.postgres.pool.max_lifetime":   "5m",
			"database.postgres.pool.max_idle_time":  "10m",
			"database.postgres.migrations.enabled":  true,
			"database.postgres.migrations.path":     "./migrations",
			"database.postgres.log_level":           "warn",

			// Services defaults
			"services.identity.enabled":   true,
			"services.identity.url":       "identity-service:50051",
			"services.identity.timeout":   "5s",
			"services.permission.enabled": true,
			"services.permission.url":     "permission-service:50054",
			"services.permission.timeout": "5s",
			"services.canvas.enabled":     true,
			"services.canvas.url":         "canvas-service:50058",
			"services.canvas.timeout":     "10s",
			// Git service HTTP origin (symbol graph + impact analysis).
			// Empty URL disables the symbol graph path in
			// AffectedTestResolver (it falls back to the filename
			// heuristic), so this is safe to leave unset locally.
			"services.git.enabled": false,
			"services.git.url":     "",
			"services.git.timeout": "10s",

			// Firecracker defaults
			"firecracker.binary_path":      "/usr/local/bin/firecracker",
			"firecracker.jailer_path":      "/usr/local/bin/jailer",
			"firecracker.kernel_path":      "/var/lib/firecracker/kernel/vmlinux",
			"firecracker.rootfs_base_path": "/var/lib/firecracker/rootfs",
			"firecracker.snapshot_path":    "/var/lib/firecracker/snapshots",
			"firecracker.socket_dir":       "/tmp/firecracker",
			"firecracker.default_vcpu":     1,
			"firecracker.default_mem_mb":   128,
			"firecracker.max_vcpu":         4,
			"firecracker.max_mem_mb":       2048,
			"firecracker.default_timeout":  "30s",
			"firecracker.max_timeout":      "300s",
			"firecracker.pool_size":        5,
			"firecracker.use_jailer":       false,
			// Short on purpose: the jail dir and the VM's socket basename both
			// land in the host socket path, which must stay under the AF_UNIX
			// sun_path limit of 107 bytes.
			"firecracker.chroot_base": "/var/lib/firecracker/j",
			"firecracker.vm_uid_base": 100000,
			"firecracker.vm_uid_span": 8192,
			// §9.1 — per-profile warm pool defaults. Per-profile size of
			// 2 mirrors the spec default; empty language list keeps
			// backwards-compat (no pool) until operators opt in.
			"firecracker.pool_size_per_profile":       2,
			"firecracker.pool_languages":              []string{},
			"firecracker.checkpoint_interval_minutes": 15,
			"firecracker.enable_checkpoint_scheduler": false,
			"firecracker.warm_pool_enabled":           false,
			"firecracker.warm_pool_ready":             0,

			// Container defaults
			"container.default_vcpu":       1,
			"container.default_mem_mb":     256,
			"container.max_vcpu":           4,
			"container.max_mem_mb":         2048,
			"container.default_timeout":    "30s",
			"container.max_timeout":        "300s",
			"container.allow_host_network": false,

			// Kafka defaults
			"kafka.enabled": false,
			"kafka.topic":   "sentiae.runtime",

			// §9.2 hermetic enforcement defaults — off so callers aren't
			// surprised by new validation.
			"hermetic.enforce_base_image_digest": false,
			"hermetic.enforce_reproducibility":   false,

			// Durable snapshot object-store defaults. Disabled by default so
			// existing deployments keep the local-only snapshot flow until
			// operators opt in. MinIO-friendly defaults (path-style, no TLS).
			"snapshot_store.enabled":    false,
			"snapshot_store.endpoint":   "minio:9000",
			"snapshot_store.bucket":     "sentiae-snapshots",
			"snapshot_store.access_key": "minioadmin",
			"snapshot_store.secret_key": "minioadmin",
			"snapshot_store.region":     "us-east-1",
			"snapshot_store.use_ssl":    false,
			"snapshot_store.path_style": true,
			"snapshot_store.cache_dir":  "/var/lib/firecracker/snapshot-cache",

			// OCI registry (image-boot pull source, D-016).
			"registry.host":        "10.0.10.20:8078",
			"registry.service_key": "",

			// Image-boot (runtime-fleet CP3).
			"imageboot.work_dir":       "/var/lib/sentiae/images",
			"imageboot.init_path":      "/usr/local/bin/image-init",
			"imageboot.host_port_min":  20000,
			"imageboot.host_port_max":  20999,
			"imageboot.advertise_host": "10.0.10.244",

			// Fleet self-registration + heartbeat (runtime-fleet CP4 §9#4).
			"fleet.host_id":               "",
			"fleet.region":                "homelab",
			"fleet.host_disk_mb":          51200,
			"fleet.heartbeat_interval":    "10s",
			"fleet.secret_selftest":       false,
			"fleet.volume_dir":            "",
			"fleet.ingress_domain":        "fleet.sentiae.local",
			"fleet.caddy.admin_endpoint":  "http://127.0.0.1:2019",
			"fleet.caddy.access_log_path": "/var/log/sentiae/caddy-access.log",
			"fleet.activate_timeout":      "30s",
			"fleet.activator_endpoint":    "127.0.0.1:8090",

			// Telemetry (OTLP export → otel-collector, D-179 Wave-8). Defaulted,
			// never required — telemetry init is non-fatal.
			"telemetry.service_name":  "runtime-service",
			"telemetry.otlp_endpoint": "otel-collector:4317",

			// P19 durable resource control plane (CP4.5 §9 #3, D-183).
			"resource.engine_pg_image_registry":   "",
			"resource.engine_pg_image_repository": "",
			"resource.engine_pg_image_digest":     "",
			"resource.conn_budget":                100,
			"resource.shared_pg_host":             "",
			"resource.shared_pg_port":             5432,
			"resource.shared_ttl":                 "24h",
			"resource.shared_seed_templates":      []string{},
		},
		BindEnvs: [][2]string{
			// App bindings
			{"app.name", "APP_APP_NAME"},
			{"app.version", "APP_VERSION"},
			{"app.environment", "APP_ENVIRONMENT"},
			{"app.executor_type", "APP_EXECUTOR_TYPE"},
			{"app.use_vsock", "APP_USE_VSOCK"},

			// Logging bindings
			{"logging.level", "APP_LOGGING_LEVEL"},
			{"logging.format", "APP_LOGGING_FORMAT"},
			{"logging.output", "APP_LOGGING_OUTPUT"},

			// HTTP server bindings
			{"server.http.enabled", "APP_SERVER_HTTP_ENABLED"},
			{"server.http.host", "APP_SERVER_HTTP_HOST"},
			{"server.http.port", "APP_SERVER_PORT"},
			{"server.http.base_path", "APP_SERVER_HTTP_BASE_PATH"},
			{"server.http.timeouts.read", "APP_SERVER_HTTP_TIMEOUTS_READ"},
			{"server.http.timeouts.write", "APP_SERVER_HTTP_TIMEOUTS_WRITE"},
			{"server.http.timeouts.idle", "APP_SERVER_HTTP_TIMEOUTS_IDLE"},
			{"server.http.timeouts.shutdown", "APP_SERVER_HTTP_TIMEOUTS_SHUTDOWN"},

			// gRPC bindings
			{"server.grpc.enabled", "APP_SERVER_GRPC_ENABLED"},
			{"server.grpc.host", "APP_SERVER_GRPC_HOST"},
			{"server.grpc.port", "APP_GRPC_PORT"},
			{"server.grpc.service_api_key", "APP_GRPC_SERVICE_API_KEY"},
			{"server.grpc.jwks_url", "APP_AUTH_JWKS_URL"},
			{"server.grpc.jwt_issuer", "APP_AUTH_JWT_ISSUER"},

			// Database bindings
			{"database.postgres.host", "APP_DATABASE_HOST"},
			{"database.postgres.port", "APP_DATABASE_PORT"},
			{"database.postgres.user", "APP_DATABASE_USER"},
			{"database.postgres.password", "APP_DATABASE_PASSWORD"},
			{"database.postgres.database", "APP_DATABASE_NAME"},
			{"database.postgres.ssl_mode", "APP_DATABASE_SSL_MODE"},
			{"database.postgres.pool.max_open_conns", "APP_DATABASE_MAX_OPEN_CONNS"},
			{"database.postgres.pool.max_idle_conns", "APP_DATABASE_MAX_IDLE_CONNS"},
			{"database.postgres.pool.max_lifetime", "APP_DATABASE_MAX_LIFETIME"},
			{"database.postgres.pool.max_idle_time", "APP_DATABASE_MAX_IDLE_TIME"},
			{"database.postgres.migrations.enabled", "APP_DATABASE_MIGRATIONS_ENABLED"},
			{"database.postgres.migrations.path", "APP_DATABASE_MIGRATIONS_PATH"},
			{"database.postgres.log_level", "APP_DATABASE_LOG_LEVEL"},

			// Services bindings
			{"services.identity.enabled", "APP_SERVICES_IDENTITY_ENABLED"},
			{"services.identity.url", "APP_SERVICES_IDENTITY_URL"},
			{"services.identity.timeout", "APP_SERVICES_IDENTITY_TIMEOUT"},
			{"services.permission.enabled", "APP_SERVICES_PERMISSION_ENABLED"},
			{"services.permission.url", "APP_SERVICES_PERMISSION_URL"},
			{"services.permission.timeout", "APP_SERVICES_PERMISSION_TIMEOUT"},
			{"services.canvas.enabled", "APP_SERVICES_CANVAS_ENABLED"},
			{"services.canvas.url", "APP_SERVICES_CANVAS_URL"},
			{"services.canvas.timeout", "APP_SERVICES_CANVAS_TIMEOUT"},
			{"services.git.enabled", "APP_SERVICES_GIT_ENABLED"},
			{"services.git.url", "APP_SERVICES_GIT_URL"},
			{"services.git.url", "GIT_SERVICE_URL"},
			{"services.git.timeout", "APP_SERVICES_GIT_TIMEOUT"},

			// Firecracker bindings
			{"firecracker.binary_path", "APP_FC_BINARY_PATH"},
			{"firecracker.jailer_path", "APP_FC_JAILER_PATH"},
			{"firecracker.kernel_path", "APP_FC_KERNEL_PATH"},
			{"firecracker.rootfs_base_path", "APP_FC_ROOTFS_BASE_PATH"},
			{"firecracker.snapshot_path", "APP_FC_SNAPSHOT_PATH"},
			{"firecracker.socket_dir", "APP_FC_SOCKET_DIR"},
			{"firecracker.default_vcpu", "APP_FC_DEFAULT_VCPU"},
			{"firecracker.default_mem_mb", "APP_FC_DEFAULT_MEM_MB"},
			{"firecracker.max_vcpu", "APP_FC_MAX_VCPU"},
			{"firecracker.max_mem_mb", "APP_FC_MAX_MEM_MB"},
			{"firecracker.default_timeout", "APP_FC_DEFAULT_TIMEOUT"},
			{"firecracker.max_timeout", "APP_FC_MAX_TIMEOUT"},
			{"firecracker.pool_size", "APP_FC_POOL_SIZE"},
			{"firecracker.use_jailer", "APP_FC_USE_JAILER"},
			{"firecracker.chroot_base", "APP_FC_CHROOT_BASE"},
			{"firecracker.vm_uid_base", "APP_FC_VM_UID_BASE"},
			{"firecracker.vm_uid_span", "APP_FC_VM_UID_SPAN"},
			// §9.1 / §9.3
			{"firecracker.pool_size_per_profile", "FIRECRACKER_POOL_SIZE"},
			{"firecracker.pool_size_per_profile", "APP_FC_POOL_SIZE_PER_PROFILE"},
			{"firecracker.pool_languages", "APP_FC_POOL_LANGUAGES"},
			{"firecracker.checkpoint_interval_minutes", "FIRECRACKER_CHECKPOINT_INTERVAL"},
			{"firecracker.checkpoint_interval_minutes", "APP_FC_CHECKPOINT_INTERVAL_MINUTES"},
			{"firecracker.enable_checkpoint_scheduler", "APP_FC_ENABLE_CHECKPOINT_SCHEDULER"},
			{"firecracker.warm_pool_enabled", "APP_WARM_POOL_ENABLED"},
			{"firecracker.warm_pool_ready", "APP_WARM_POOL_READY"},

			// Container bindings
			{"container.default_vcpu", "APP_CONTAINER_DEFAULT_VCPU"},
			{"container.default_mem_mb", "APP_CONTAINER_DEFAULT_MEM_MB"},
			{"container.max_vcpu", "APP_CONTAINER_MAX_VCPU"},
			{"container.max_mem_mb", "APP_CONTAINER_MAX_MEM_MB"},
			{"container.default_timeout", "APP_CONTAINER_DEFAULT_TIMEOUT"},
			{"container.max_timeout", "APP_CONTAINER_MAX_TIMEOUT"},
			{"container.allow_host_network", "APP_CONTAINER_ALLOW_HOST_NETWORK"},

			// Kafka bindings
			{"kafka.enabled", "APP_KAFKA_ENABLED"},
			{"kafka.brokers", "APP_KAFKA_BROKERS"},
			{"kafka.topic", "APP_KAFKA_TOPIC"},

			// §9.2 hermetic enforcement
			{"hermetic.enforce_base_image_digest", "HERMETIC_REQUIRE_BASE_IMAGE_DIGEST"},
			{"hermetic.enforce_base_image_digest", "APP_HERMETIC_ENFORCE_BASE_IMAGE_DIGEST"},
			{"hermetic.enforce_reproducibility", "HERMETIC_ENFORCE_REPRODUCIBILITY"},
			{"hermetic.enforce_reproducibility", "APP_HERMETIC_ENFORCE_REPRODUCIBILITY"},

			// Durable snapshot object store
			{"snapshot_store.enabled", "APP_SNAPSHOT_STORE_ENABLED"},
			{"snapshot_store.endpoint", "APP_SNAPSHOT_STORE_ENDPOINT"},
			{"snapshot_store.bucket", "APP_SNAPSHOT_STORE_BUCKET"},
			{"snapshot_store.access_key", "APP_SNAPSHOT_STORE_ACCESS_KEY"},
			{"snapshot_store.secret_key", "APP_SNAPSHOT_STORE_SECRET_KEY"},
			{"snapshot_store.region", "APP_SNAPSHOT_STORE_REGION"},
			{"snapshot_store.use_ssl", "APP_SNAPSHOT_STORE_USE_SSL"},
			{"snapshot_store.path_style", "APP_SNAPSHOT_STORE_PATH_STYLE"},
			{"snapshot_store.cache_dir", "APP_SNAPSHOT_STORE_CACHE_DIR"},

			// OCI registry
			{"registry.host", "APP_REGISTRY_HOST"},
			{"registry.service_key", "APP_REGISTRY_SERVICE_KEY"},

			// Image-boot (runtime-fleet CP3)
			{"imageboot.work_dir", "APP_IMAGEBOOT_WORKDIR"},
			{"imageboot.init_path", "APP_IMAGEBOOT_INIT_PATH"},
			{"imageboot.host_port_min", "APP_IMAGEBOOT_HOST_PORT_MIN"},
			{"imageboot.host_port_max", "APP_IMAGEBOOT_HOST_PORT_MAX"},
			{"imageboot.advertise_host", "APP_IMAGEBOOT_ADVERTISE_HOST"},

			// Fleet self-registration + heartbeat (runtime-fleet CP4)
			{"fleet.host_id", "APP_FLEET_HOST_ID"},
			{"fleet.region", "APP_FLEET_REGION"},
			{"fleet.host_disk_mb", "APP_FLEET_HOST_DISK_MB"},
			{"fleet.heartbeat_interval", "APP_FLEET_HEARTBEAT_INTERVAL"},
			{"fleet.secret_selftest", "APP_FLEET_SECRET_SELFTEST"},
			{"fleet.volume_dir", "APP_FLEET_VOLUME_DIR"},
			{"fleet.ingress_domain", "APP_FLEET_INGRESS_DOMAIN"},
			{"fleet.caddy.admin_endpoint", "APP_FLEET_CADDY_ADMIN_ENDPOINT"},
			{"fleet.caddy.access_log_path", "APP_FLEET_CADDY_ACCESS_LOG_PATH"},
			{"fleet.activate_timeout", "APP_FLEET_ACTIVATE_TIMEOUT"},
			{"fleet.activator_endpoint", "APP_FLEET_ACTIVATOR_ENDPOINT"},

			// Telemetry (OTLP export)
			{"telemetry.service_name", "APP_TELEMETRY_SERVICE_NAME"},
			{"telemetry.otlp_endpoint", "APP_TELEMETRY_OTLP_ENDPOINT"},

			// P19 durable resource control plane (CP4.5 §9 #3, D-183)
			{"resource.engine_pg_image_registry", "APP_RESOURCE_ENGINE_PG_IMAGE_REGISTRY"},
			{"resource.engine_pg_image_repository", "APP_RESOURCE_ENGINE_PG_IMAGE_REPOSITORY"},
			{"resource.engine_pg_image_digest", "APP_RESOURCE_ENGINE_PG_IMAGE_DIGEST"},
			{"resource.conn_budget", "APP_RESOURCE_CONN_BUDGET"},
			{"resource.shared_pg_host", "APP_RESOURCE_SHARED_PG_HOST"},
			{"resource.shared_pg_port", "APP_RESOURCE_SHARED_PG_PORT"},
			{"resource.shared_ttl", "APP_RESOURCE_SHARED_TTL"},
			{"resource.shared_seed_templates", "APP_RESOURCE_SHARED_SEED_TEMPLATES"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}

	// Clamp the pre-warm buffer depth into [0, maxWarmPoolReady] so a negative
	// or oversized value can never start a runaway replenisher or exhaust the
	// host. 0 keeps the on-demand-only behavior.
	if cfg.Firecracker.WarmPoolReady < 0 {
		cfg.Firecracker.WarmPoolReady = 0
	}
	if cfg.Firecracker.WarmPoolReady > maxWarmPoolReady {
		cfg.Firecracker.WarmPoolReady = maxWarmPoolReady
	}

	return &cfg, nil
}

// GetDatabaseURL returns the PostgreSQL connection URL.
func (c *Config) GetDatabaseURL() string {
	return fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		c.Database.Postgres.Host,
		c.Database.Postgres.Port,
		c.Database.Postgres.User,
		c.Database.Postgres.Password,
		c.Database.Postgres.Database,
		c.Database.Postgres.SSLMode,
	)
}
