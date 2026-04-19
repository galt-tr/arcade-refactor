package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/bsv-blockchain/arcade/config"
	"github.com/bsv-blockchain/arcade/kafka"
	"github.com/bsv-blockchain/arcade/merkleservice"
	"github.com/bsv-blockchain/arcade/services"
	"github.com/bsv-blockchain/arcade/services/api_server"
	"github.com/bsv-blockchain/arcade/services/bump_builder"
	"github.com/bsv-blockchain/arcade/services/propagation"
	"github.com/bsv-blockchain/arcade/services/tx_validator"
	"github.com/bsv-blockchain/arcade/store"
	"github.com/bsv-blockchain/arcade/teranode"
	"github.com/bsv-blockchain/arcade/validator"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "arcade",
		Short: "Arcade transaction management service",
		RunE:  run,
	}

	config.BindFlags(rootCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load(cmd)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	logger := newLogger(cfg.LogLevel)
	defer logger.Sync()

	logger.Info("starting arcade", zap.String("mode", cfg.Mode))

	producer, err := kafka.NewProducer(cfg.Kafka.Brokers)
	if err != nil {
		return fmt.Errorf("creating kafka producer: %w", err)
	}
	defer producer.Close()

	aeroStore, err := store.NewAerospikeStore(cfg.Aerospike)
	if err != nil {
		return fmt.Errorf("connecting to aerospike: %w", err)
	}
	defer aeroStore.Close()

	// Create shared components
	txTracker := store.NewTxTracker()

	teranodeClient := teranode.NewClient(cfg.DatahubURLs, cfg.Teranode.AuthToken)

	var merkleClient *merkleservice.Client
	if cfg.MerkleService.URL != "" {
		merkleClient = merkleservice.NewClient(cfg.MerkleService.URL, cfg.MerkleService.AuthToken, 0)
		merkleClient.SetLogger(logger.Named("merkle-client"))
	}

	txVal := validator.NewValidator(nil, nil) // Default policy, no chain tracker yet

	svcs := buildServices(cfg, logger, producer, aeroStore, txTracker, teranodeClient, merkleClient, txVal)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start health server for non-API modes (api-server serves /health on its own port)
	if cfg.Mode != "api-server" {
		hs := services.NewHealthServer(cfg.Health.Port, logger)
		hs.Start(ctx)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	var wg sync.WaitGroup
	errCh := make(chan error, len(svcs))

	for _, svc := range svcs {
		wg.Add(1)
		go func(s services.Service) {
			defer wg.Done()
			logger.Info("starting service", zap.String("service", s.Name()))
			if err := s.Start(ctx); err != nil {
				logger.Error("service failed", zap.String("service", s.Name()), zap.Error(err))
				errCh <- fmt.Errorf("service %s: %w", s.Name(), err)
			}
		}(svc)
	}

	select {
	case sig := <-sigCh:
		logger.Info("received signal, shutting down", zap.String("signal", sig.String()))
	case err := <-errCh:
		logger.Error("service error, shutting down", zap.Error(err))
	}

	cancel()

	for _, svc := range svcs {
		logger.Info("stopping service", zap.String("service", svc.Name()))
		if err := svc.Stop(); err != nil {
			logger.Error("error stopping service", zap.String("service", svc.Name()), zap.Error(err))
		}
	}

	wg.Wait()
	logger.Info("arcade stopped")
	return nil
}

func buildServices(
	cfg *config.Config,
	logger *zap.Logger,
	producer *kafka.Producer,
	aeroStore *store.AerospikeStore,
	txTracker *store.TxTracker,
	teranodeClient *teranode.Client,
	merkleClient *merkleservice.Client,
	txVal *validator.Validator,
) []services.Service {
	var svcs []services.Service

	shouldRun := func(name string) bool {
		return cfg.Mode == "all" || cfg.Mode == name
	}

	if shouldRun("api-server") {
		svcs = append(svcs, api_server.New(cfg, logger, producer, aeroStore, txTracker))
	}
	if shouldRun("bump-builder") {
		svcs = append(svcs, bump_builder.New(cfg, logger, aeroStore))
	}
	if shouldRun("tx-validator") {
		svcs = append(svcs, tx_validator.New(cfg, logger, producer, aeroStore, txTracker, txVal))
	}
	if shouldRun("propagation") {
		// aeroStore satisfies both store.Store and store.Leaser; pass it twice
		// so a future backend can be split across the two interfaces.
		svcs = append(svcs, propagation.New(cfg, logger, producer, aeroStore, aeroStore, teranodeClient, merkleClient))
	}

	return svcs
}

func newLogger(level string) *zap.Logger {
	var zapLevel zapcore.Level
	switch level {
	case "debug":
		zapLevel = zap.DebugLevel
	case "warn":
		zapLevel = zap.WarnLevel
	case "error":
		zapLevel = zap.ErrorLevel
	default:
		zapLevel = zap.InfoLevel
	}

	cfg := zap.Config{
		Level:            zap.NewAtomicLevelAt(zapLevel),
		Encoding:         "json",
		OutputPaths:      []string{"stdout"},
		ErrorOutputPaths: []string{"stderr"},
		EncoderConfig:    zap.NewProductionEncoderConfig(),
	}

	logger, _ := cfg.Build()
	return logger
}
