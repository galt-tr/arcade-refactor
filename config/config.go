package config

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

type Config struct {
	Mode          string   `mapstructure:"mode"`
	LogLevel      string   `mapstructure:"log_level"`
	CallbackURL   string   `mapstructure:"callback_url"`
	CallbackToken string   `mapstructure:"callback_token"`
	APIServer     API      `mapstructure:"api"`
	Kafka         Kafka    `mapstructure:"kafka"`
	Aerospike     Aero     `mapstructure:"aerospike"`
	DatahubURLs   []string `mapstructure:"datahub_urls"`
	Teranode      TeranodeConfig      `mapstructure:"teranode"`
	MerkleService MerkleServiceConfig `mapstructure:"merkle_service"`
	P2P           P2PConfig           `mapstructure:"p2p"`
	Health        HealthConfig        `mapstructure:"health"`
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

func BindFlags(cmd *cobra.Command) {
	cmd.Flags().String("mode", "all", "Service mode: all, api-server, p2p-client, block-processor, bump-builder, tx-validator, propagation")
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
}

func validate(cfg *Config) error {
	if len(cfg.Kafka.Brokers) == 0 {
		return fmt.Errorf("kafka.brokers is required")
	}
	if len(cfg.Aerospike.Hosts) == 0 {
		return fmt.Errorf("aerospike.hosts is required")
	}
	validModes := map[string]bool{
		"all": true, "api-server": true, "p2p-client": true,
		"block-processor": true, "bump-builder": true,
		"tx-validator": true, "propagation": true,
	}
	if !validModes[cfg.Mode] {
		return fmt.Errorf("invalid mode %q", cfg.Mode)
	}
	return nil
}
