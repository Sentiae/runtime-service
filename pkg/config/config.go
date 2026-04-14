package config

import (
	"fmt"
	"time"

	pkconfig "github.com/sentiae/platform-kit/config"
)

// Config represents the complete application configuration.
type Config struct {
	App         AppConfig         `mapstructure:"app"`
	Logging     LoggingConfig     `mapstructure:"logging"`
	Server      ServerConfig      `mapstructure:"server"`
	Database    DatabaseConfig    `mapstructure:"database"`
	Services    ServicesConfig    `mapstructure:"services"`
	Firecracker FirecrackerConfig `mapstructure:"firecracker"`
	Container   ContainerConfig   `mapstructure:"container"`
	Kafka       KafkaConfig       `mapstructure:"kafka"`
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
	Enabled     bool   `mapstructure:"enabled"`
	Path        string `mapstructure:"path"`
	AutoMigrate bool   `mapstructure:"auto_migrate"`
}

// ServicesConfig contains external service endpoints.
type ServicesConfig struct {
	Identity   ServiceEndpoint `mapstructure:"identity"`
	Permission ServiceEndpoint `mapstructure:"permission"`
	Canvas     ServiceEndpoint `mapstructure:"canvas"`
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
}

// ContainerConfig contains Docker container executor configuration.
type ContainerConfig struct {
	DefaultVCPU    int           `mapstructure:"default_vcpu"`
	DefaultMemMB   int           `mapstructure:"default_mem_mb"`
	MaxVCPU        int           `mapstructure:"max_vcpu"`
	MaxMemMB       int           `mapstructure:"max_mem_mb"`
	DefaultTimeout time.Duration `mapstructure:"default_timeout"`
	MaxTimeout     time.Duration `mapstructure:"max_timeout"`
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
			"server.grpc.enabled": true,
			"server.grpc.host":    "0.0.0.0",
			"server.grpc.port":    "50060",

			// Database defaults
			"database.postgres.host":                    "localhost",
			"database.postgres.port":                    "5432",
			"database.postgres.user":                    "postgres",
			"database.postgres.password":                "postgres",
			"database.postgres.database":                "runtime_service",
			"database.postgres.ssl_mode":                "disable",
			"database.postgres.pool.max_open_conns":     25,
			"database.postgres.pool.max_idle_conns":     10,
			"database.postgres.pool.max_lifetime":       "5m",
			"database.postgres.pool.max_idle_time":      "10m",
			"database.postgres.migrations.enabled":      true,
			"database.postgres.migrations.path":         "./migrations",
			"database.postgres.migrations.auto_migrate": false,
			"database.postgres.log_level":               "warn",

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

			// Container defaults
			"container.default_vcpu":    1,
			"container.default_mem_mb":  256,
			"container.max_vcpu":        4,
			"container.max_mem_mb":      2048,
			"container.default_timeout": "30s",
			"container.max_timeout":     "300s",

			// Kafka defaults
			"kafka.enabled": false,
			"kafka.topic":   "sentiae.runtime",
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
			{"database.postgres.migrations.auto_migrate", "APP_DATABASE_AUTO_MIGRATE"},
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

			// Container bindings
			{"container.default_vcpu", "APP_CONTAINER_DEFAULT_VCPU"},
			{"container.default_mem_mb", "APP_CONTAINER_DEFAULT_MEM_MB"},
			{"container.max_vcpu", "APP_CONTAINER_MAX_VCPU"},
			{"container.max_mem_mb", "APP_CONTAINER_MAX_MEM_MB"},
			{"container.default_timeout", "APP_CONTAINER_DEFAULT_TIMEOUT"},
			{"container.max_timeout", "APP_CONTAINER_MAX_TIMEOUT"},

			// Kafka bindings
			{"kafka.enabled", "APP_KAFKA_ENABLED"},
			{"kafka.brokers", "APP_KAFKA_BROKERS"},
			{"kafka.topic", "APP_KAFKA_TOPIC"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
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
