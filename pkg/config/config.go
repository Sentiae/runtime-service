package config

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config represents the complete application configuration
type Config struct {
	App         AppConfig         `yaml:"app"`
	Logging     LoggingConfig     `yaml:"logging"`
	Server      ServerConfig      `yaml:"server"`
	Database    DatabaseConfig    `yaml:"database"`
	Services    ServicesConfig    `yaml:"services"`
	Firecracker FirecrackerConfig `yaml:"firecracker"`
	Kafka       KafkaConfig       `yaml:"kafka"`
}

// KafkaConfig contains Kafka event publishing configuration
type KafkaConfig struct {
	Enabled bool     `yaml:"enabled"`
	Brokers []string `yaml:"brokers"`
	Topic   string   `yaml:"topic"`
}

// AppConfig contains application metadata
type AppConfig struct {
	Name        string `yaml:"name"`
	Version     string `yaml:"version"`
	Environment string `yaml:"environment"`
}

// LoggingConfig contains logging configuration
type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
	Output string `yaml:"output"`
}

// ServerConfig contains server configuration
type ServerConfig struct {
	HTTP HTTPConfig `yaml:"http"`
	GRPC GRPCConfig `yaml:"grpc"`
}

// HTTPConfig contains HTTP server configuration
type HTTPConfig struct {
	Enabled  bool           `yaml:"enabled"`
	Host     string         `yaml:"host"`
	Port     string         `yaml:"port"`
	BasePath string         `yaml:"base_path"`
	Timeouts TimeoutsConfig `yaml:"timeouts"`
}

// TimeoutsConfig contains timeout settings
type TimeoutsConfig struct {
	Read     time.Duration `yaml:"read"`
	Write    time.Duration `yaml:"write"`
	Idle     time.Duration `yaml:"idle"`
	Shutdown time.Duration `yaml:"shutdown"`
}

// GRPCConfig contains gRPC server configuration
type GRPCConfig struct {
	Enabled bool   `yaml:"enabled"`
	Host    string `yaml:"host"`
	Port    string `yaml:"port"`
}

// DatabaseConfig contains database configuration
type DatabaseConfig struct {
	Postgres PostgresConfig `yaml:"postgres"`
}

// PostgresConfig contains PostgreSQL configuration
type PostgresConfig struct {
	Host       string           `yaml:"host"`
	Port       string           `yaml:"port"`
	User       string           `yaml:"user"`
	Password   string           `yaml:"password"`
	Database   string           `yaml:"database"`
	SSLMode    string           `yaml:"ssl_mode"`
	Pool       PoolConfig       `yaml:"pool"`
	Migrations MigrationsConfig `yaml:"migrations"`
	LogLevel   string           `yaml:"log_level"`
}

// PoolConfig contains connection pool settings
type PoolConfig struct {
	MaxOpenConns int           `yaml:"max_open_conns"`
	MaxIdleConns int           `yaml:"max_idle_conns"`
	MaxLifetime  time.Duration `yaml:"max_lifetime"`
	MaxIdleTime  time.Duration `yaml:"max_idle_time"`
}

// MigrationsConfig contains migration settings
type MigrationsConfig struct {
	Enabled     bool   `yaml:"enabled"`
	Path        string `yaml:"path"`
	AutoMigrate bool   `yaml:"auto_migrate"`
}

// ServicesConfig contains external service endpoints
type ServicesConfig struct {
	Identity   ServiceEndpoint `yaml:"identity"`
	Permission ServiceEndpoint `yaml:"permission"`
	Canvas     ServiceEndpoint `yaml:"canvas"`
}

// ServiceEndpoint represents an external service configuration
type ServiceEndpoint struct {
	Enabled bool          `yaml:"enabled"`
	URL     string        `yaml:"url"`
	Timeout time.Duration `yaml:"timeout"`
}

// FirecrackerConfig contains Firecracker microVM configuration
type FirecrackerConfig struct {
	BinaryPath     string        `yaml:"binary_path"`
	JailerPath     string        `yaml:"jailer_path"`
	KernelPath     string        `yaml:"kernel_path"`
	RootfsBasePath string        `yaml:"rootfs_base_path"`
	SnapshotPath   string        `yaml:"snapshot_path"`
	SocketDir      string        `yaml:"socket_dir"`
	DefaultVCPU    int           `yaml:"default_vcpu"`
	DefaultMemMB   int           `yaml:"default_mem_mb"`
	MaxVCPU        int           `yaml:"max_vcpu"`
	MaxMemMB       int           `yaml:"max_mem_mb"`
	DefaultTimeout time.Duration `yaml:"default_timeout"`
	MaxTimeout     time.Duration `yaml:"max_timeout"`
	PoolSize       int           `yaml:"pool_size"`
	UseJailer      bool          `yaml:"use_jailer"`
}

// Load loads configuration from environment variables with defaults
func Load() *Config {
	cfg := &Config{
		App: AppConfig{
			Name:        getEnv("APP_NAME", "runtime-service"),
			Version:     getEnv("VERSION", "dev"),
			Environment: getEnv("ENVIRONMENT", "development"),
		},
		Logging: LoggingConfig{
			Level:  getEnv("LOG_LEVEL", "info"),
			Format: getEnv("LOG_FORMAT", "json"),
			Output: getEnv("LOG_OUTPUT", "stdout"),
		},
		Server: ServerConfig{
			HTTP: HTTPConfig{
				Enabled:  getEnvAsBool("SERVER_HTTP_ENABLED", true),
				Host:     getEnv("SERVER_HOST", "0.0.0.0"),
				Port:     getEnv("PORT", "8090"),
				BasePath: getEnv("SERVER_BASE_PATH", "/api/v1"),
				Timeouts: TimeoutsConfig{
					Read:     getEnvAsDuration("SERVER_READ_TIMEOUT", "30s"),
					Write:    getEnvAsDuration("SERVER_WRITE_TIMEOUT", "120s"),
					Idle:     getEnvAsDuration("SERVER_IDLE_TIMEOUT", "60s"),
					Shutdown: getEnvAsDuration("SERVER_SHUTDOWN_TIMEOUT", "30s"),
				},
			},
			GRPC: GRPCConfig{
				Enabled: getEnvAsBool("GRPC_ENABLED", true),
				Host:    getEnv("GRPC_HOST", "0.0.0.0"),
				Port:    getEnv("GRPC_PORT", "50060"),
			},
		},
		Database: DatabaseConfig{
			Postgres: PostgresConfig{
				Host:     getEnv("DB_HOST", "localhost"),
				Port:     getEnv("DB_PORT", "5432"),
				User:     getEnv("DB_USER", "postgres"),
				Password: getEnv("DB_PASSWORD", "postgres"),
				Database: getEnv("DB_NAME", "runtime_service"),
				SSLMode:  getEnv("DB_SSL_MODE", "disable"),
				Pool: PoolConfig{
					MaxOpenConns: getEnvAsInt("DB_MAX_OPEN_CONNS", 25),
					MaxIdleConns: getEnvAsInt("DB_MAX_IDLE_CONNS", 10),
					MaxLifetime:  getEnvAsDuration("DB_MAX_LIFETIME", "5m"),
					MaxIdleTime:  getEnvAsDuration("DB_MAX_IDLE_TIME", "10m"),
				},
				Migrations: MigrationsConfig{
					Enabled:     getEnvAsBool("DB_MIGRATIONS_ENABLED", true),
					Path:        getEnv("DB_MIGRATIONS_PATH", "./migrations"),
					AutoMigrate: getEnvAsBool("DB_AUTO_MIGRATE", false),
				},
				LogLevel: getEnv("DB_LOG_LEVEL", "warn"),
			},
		},
		Services: ServicesConfig{
			Identity: ServiceEndpoint{
				Enabled: getEnvAsBool("IDENTITY_SERVICE_ENABLED", true),
				URL:     getEnv("IDENTITY_SERVICE_URL", "identity-service:50051"),
				Timeout: getEnvAsDuration("IDENTITY_SERVICE_TIMEOUT", "5s"),
			},
			Permission: ServiceEndpoint{
				Enabled: getEnvAsBool("PERMISSION_SERVICE_ENABLED", true),
				URL:     getEnv("PERMISSION_SERVICE_URL", "permission-service:50054"),
				Timeout: getEnvAsDuration("PERMISSION_SERVICE_TIMEOUT", "5s"),
			},
			Canvas: ServiceEndpoint{
				Enabled: getEnvAsBool("CANVAS_SERVICE_ENABLED", true),
				URL:     getEnv("CANVAS_SERVICE_URL", "canvas-service:50058"),
				Timeout: getEnvAsDuration("CANVAS_SERVICE_TIMEOUT", "10s"),
			},
		},
		Firecracker: FirecrackerConfig{
			BinaryPath:     getEnv("FC_BINARY_PATH", "/usr/local/bin/firecracker"),
			JailerPath:     getEnv("FC_JAILER_PATH", "/usr/local/bin/jailer"),
			KernelPath:     getEnv("FC_KERNEL_PATH", "/var/lib/firecracker/kernel/vmlinux"),
			RootfsBasePath: getEnv("FC_ROOTFS_BASE_PATH", "/var/lib/firecracker/rootfs"),
			SnapshotPath:   getEnv("FC_SNAPSHOT_PATH", "/var/lib/firecracker/snapshots"),
			SocketDir:      getEnv("FC_SOCKET_DIR", "/tmp/firecracker"),
			DefaultVCPU:    getEnvAsInt("FC_DEFAULT_VCPU", 1),
			DefaultMemMB:   getEnvAsInt("FC_DEFAULT_MEM_MB", 128),
			MaxVCPU:        getEnvAsInt("FC_MAX_VCPU", 4),
			MaxMemMB:       getEnvAsInt("FC_MAX_MEM_MB", 2048),
			DefaultTimeout: getEnvAsDuration("FC_DEFAULT_TIMEOUT", "30s"),
			MaxTimeout:     getEnvAsDuration("FC_MAX_TIMEOUT", "300s"),
			PoolSize:       getEnvAsInt("FC_POOL_SIZE", 5),
			UseJailer:      getEnvAsBool("FC_USE_JAILER", false),
		},
	}

	// Kafka configuration
	kafkaBrokers := getEnv("KAFKA_BROKERS", "")
	var brokers []string
	if kafkaBrokers != "" {
		for _, b := range splitAndTrim(kafkaBrokers, ",") {
			if b != "" {
				brokers = append(brokers, b)
			}
		}
	}
	cfg.Kafka = KafkaConfig{
		Enabled: getEnvAsBool("KAFKA_ENABLED", false),
		Brokers: brokers,
		Topic:   getEnv("KAFKA_TOPIC", "sentiae.runtime"),
	}

	return cfg
}

// GetDatabaseURL returns the PostgreSQL connection URL
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

// Helper functions for environment variable parsing
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvAsInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intValue, err := strconv.Atoi(value); err == nil {
			return intValue
		}
		log.Printf("Warning: Invalid integer value for %s: %s, using default: %d", key, value, defaultValue)
	}
	return defaultValue
}

func getEnvAsBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		if boolValue, err := strconv.ParseBool(value); err == nil {
			return boolValue
		}
		log.Printf("Warning: Invalid boolean value for %s: %s, using default: %t", key, value, defaultValue)
	}
	return defaultValue
}

func splitAndTrim(s, sep string) []string {
	parts := strings.Split(s, sep)
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func getEnvAsDuration(key string, defaultValue string) time.Duration {
	if value := os.Getenv(key); value != "" {
		if duration, err := time.ParseDuration(value); err == nil {
			return duration
		}
		log.Printf("Warning: Invalid duration value for %s: %s, using default: %s", key, value, defaultValue)
	}
	duration, _ := time.ParseDuration(defaultValue)
	return duration
}
