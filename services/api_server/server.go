package api_server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/bsv-blockchain/arcade/config"
	"github.com/bsv-blockchain/arcade/kafka"
	"github.com/bsv-blockchain/arcade/store"
)

type Server struct {
	cfg       *config.Config
	logger    *zap.Logger
	producer  *kafka.Producer
	store     store.Store
	txTracker *store.TxTracker
	server    *http.Server
}

func New(cfg *config.Config, logger *zap.Logger, producer *kafka.Producer, st store.Store, tracker *store.TxTracker) *Server {
	return &Server{
		cfg:       cfg,
		logger:    logger.Named("api-server"),
		producer:  producer,
		store:     st,
		txTracker: tracker,
	}
}

func (s *Server) Name() string { return "api-server" }

func (s *Server) Start(ctx context.Context) error {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())
	router.Use(s.requestLogger())

	s.registerRoutes(router)

	addr := fmt.Sprintf("%s:%d", s.cfg.APIServer.Host, s.cfg.APIServer.Port)
	s.server = &http.Server{
		Addr:    addr,
		Handler: router,
	}

	s.logger.Info("API server listening", zap.String("addr", addr))

	go func() {
		<-ctx.Done()
		s.Stop()
	}()

	if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server error: %w", err)
	}
	return nil
}

func (s *Server) requestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		s.logger.Debug("request",
			zap.String("method", c.Request.Method),
			zap.String("path", c.Request.URL.Path),
			zap.Int("status", c.Writer.Status()),
			zap.Duration("latency", time.Since(start)),
			zap.String("client_ip", c.ClientIP()),
		)
	}
}

func (s *Server) Stop() error {
	if s.server != nil {
		s.logger.Info("shutting down API server")
		return s.server.Close()
	}
	return nil
}
