package config

import (
	"fmt"
	"strings"

	chaintracksconfig "github.com/bsv-blockchain/go-chaintracks/config"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

type Config struct {
	Mode          string              `mapstructure:"mode"`
	LogLevel      string              `mapstructure:"log_level"`
	CallbackURL   string              `mapstructure:"callback_url"`
	CallbackToken string              `mapstructure:"callback_token"`
	StoragePath   string              `mapstructure:"storage_path"`
	APIServer     API                 `mapstructure:"api"`
	Kafka         Kafka               `mapstructure:"kafka"`
	Aerospike     Aero                `mapstructure:"aerospike"`
	DatahubURLs   []string            `mapstructure:"datahub_urls"`
	Teranode      TeranodeConfig      `mapstructure:"teranode"`
	MerkleService MerkleServiceConfig `mapstructure:"merkle_service"`
	P2P           P2PConfig           `mapstructure:"p2p"`
	Health        HealthConfig        `mapstructure:"health"`
	Propagation   PropagationConfig   `mapstructure:"propagation"`
	BumpBuilder   BumpBuilderConfig   `mapstructure:"bump_builder"`
	// ChaintracksServer gates whether the embedded go-chaintracks HTTP API
	// runs alongside api-server. Default is on so the refactor is a drop-in
	// replacement for the original single-binary arcade.
	ChaintracksServer ChaintracksServerConfig `mapstructure:"chaintracks_server"`
	// Chaintracks is the upstream go-chaintracks config; defaults delegated
	// to the library's own SetDefaults so new fields flow through automatically.
	Chaintracks chaintracksconfig.Config `mapstructure:"chaintracks"`
}

// ChaintracksServerConfig toggles the chaintracks HTTP surface. Separate from
// Chaintracks itself so the instance can be disabled without wiping the
// library's config block.
type ChaintracksServerConfig struct {
	Enabled bool `mapstructure:"enabled"`
}

type API struct {
	Host string `mapstructure:"host"`
	Port int    `mapstructure:"port"`
}

type Kafka struct {
	Brokers       []string `mapstructure:"brokers"`
	ConsumerGroup string   `mapstructure:"consumer_group"`
	MaxRetries    int      `mapstructure:"max_retries"`
	BufferSize    int      `mapstructure:"buffer_size"`
}

type Aero struct {
	Hosts     []string `mapstructure:"hosts"`
	Namespace string   `mapstructure:"namespace"`
	BatchSize int      `mapstructure:"batch_size"`
	PoolSize  int      `mapstructure:"pool_size"`
}

type TeranodeConfig struct {
	AuthToken string `mapstructure:"auth_token"`
}

type MerkleServiceConfig struct {
	URL       string `mapstructure:"url"`
	AuthToken string `mapstructure:"auth_token"`
}

type P2PConfig struct {
	Seeds []string `mapstructure:"seeds"`
}

type HealthConfig struct {
	Port int `mapstructure:"port"`
}

type PropagationConfig struct {
	MerkleConcurrency int `mapstructure:"merkle_concurrency"`
	RetryMaxAttempts  int `mapstructure:"retry_max_attempts"`
	RetryBackoffMs    int `mapstructure:"retry_backoff_ms"`
	ReaperIntervalMs  int `mapstructure:"reaper_interval_ms"`
	ReaperBatchSize   int `mapstructure:"reaper_batch_size"`
	// LeaseTTLMs bounds how long the reaper lease remains valid without a
	// renewal. Set to at least 2–3× reaper_interval_ms so a missed tick
	// doesn't trigger a false-positive failover. Defaults to 3× interval.
	LeaseTTLMs int `mapstructure:"lease_ttl_ms"`
	// TeranodeMaxBatchSize caps the number of transactions per POST /txs call.
	// Teranode rejects oversized batches with "too many transactions" (400),
	// which previously cascaded into a 1k+ per-tx fallback storm. Splitting
	// into chunks keeps the batch endpoint in play even under Kafka backlog.
	TeranodeMaxBatchSize int `mapstructure:"teranode_max_batch_size"`
}

// BumpBuilderConfig controls the BUMP construction workflow. GraceWindowMs is the
// delay applied after receiving BLOCK_PROCESSED before reading STUMPs from the store,
// giving merkle-service retries time to land for any STUMPs that initially got a 5xx.
type BumpBuilderConfig struct {
	GraceWindowMs int `mapstructure:"grace_window_ms"`
}

func BindFlags(cmd *cobra.Command) {
	cmd.Flags().String("mode", "all", "Service mode: all, api-server, bump-builder, tx-validator, propagation")
	cmd.Flags().String("config", "", "Path to config file")
	cmd.Flags().String("log-level", "info", "Log level: debug, info, warn, error")
	_ = viper.BindPFlag("mode", cmd.Flags().Lookup("mode"))
	_ = viper.BindPFlag("log_level", cmd.Flags().Lookup("log-level"))
}

func Load(cmd *cobra.Command) (*Config, error) {
	cfgFile, _ := cmd.Flags().GetString("config")
	if cfgFile != "" {
		viper.SetConfigFile(cfgFile)
	} else {
		viper.SetConfigName("config")
		viper.SetConfigType("yaml")
		viper.AddConfigPath(".")
		viper.AddConfigPath("/etc/arcade")
	}

	viper.SetEnvPrefix("ARCADE")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()

	setDefaults()

	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("reading config: %w", err)
		}
	}

	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshaling config: %w", err)
	}

	if err := validate(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func setDefaults() {
	viper.SetDefault("mode", "all")
	viper.SetDefault("log_level", "info")
	viper.SetDefault("api.host", "0.0.0.0")
	viper.SetDefault("api.port", 8080)
	viper.SetDefault("kafka.brokers", []string{"localhost:9092"})
	viper.SetDefault("kafka.consumer_group", "arcade")
	viper.SetDefault("kafka.max_retries", 5)
	viper.SetDefault("kafka.buffer_size", 10000)
	viper.SetDefault("aerospike.hosts", []string{"localhost:3000"})
	viper.SetDefault("aerospike.namespace", "arcade")
	viper.SetDefault("aerospike.batch_size", 500)
	viper.SetDefault("aerospike.pool_size", 256)
	viper.SetDefault("health.port", 8081)
	viper.SetDefault("propagation.merkle_concurrency", 10)
	viper.SetDefault("propagation.retry_max_attempts", 5)
	viper.SetDefault("propagation.retry_backoff_ms", 500)
	viper.SetDefault("propagation.reaper_interval_ms", 30000)
	viper.SetDefault("propagation.reaper_batch_size", 500)
	// 0 keeps New()'s 3×reaper_interval default, so changing reaper_interval
	// automatically moves the lease TTL unless the operator opts into a fixed value.
	viper.SetDefault("propagation.lease_ttl_ms", 0)
	viper.SetDefault("propagation.teranode_max_batch_size", 100)

	viper.SetDefault("storage_path", "~/.arcade")
	viper.SetDefault("chaintracks_server.enabled", true)
	// Delegate chaintracks-library defaults (mode, network, bootstrap, p2p, …)
	// to the upstream SetDefaults so any new fields are picked up automatically.
	var ct chaintracksconfig.Config
	ct.SetDefaults(viper.GetViper(), "chaintracks")
	viper.SetDefault("bump_builder.grace_window_ms", 30000)
}

func validate(cfg *Config) error {
	if len(cfg.Kafka.Brokers) == 0 {
		return fmt.Errorf("kafka.brokers is required")
	}
	if len(cfg.Aerospike.Hosts) == 0 {
		return fmt.Errorf("aerospike.hosts is required")
	}
	validModes := map[string]bool{
		"all": true, "api-server": true,
		"bump-builder": true,
		"tx-validator": true, "propagation": true,
	}
	if !validModes[cfg.Mode] {
		return fmt.Errorf("invalid mode %q", cfg.Mode)
	}
	return nil
}
