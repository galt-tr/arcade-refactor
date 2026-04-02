package api_server

import (
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/bsv-blockchain/arcade/bump"
	"github.com/bsv-blockchain/arcade/kafka"
	"github.com/bsv-blockchain/arcade/models"
)

func (s *Server) handleHealth(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (s *Server) handleReady(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ready"})
}

// handleCallback processes inbound callbacks from Merkle Service.
// Uses CallbackMessage format with Type field.
func (s *Server) handleCallback(c *gin.Context) {
	// Bearer token validation
	if s.cfg.CallbackToken != "" {
		auth := c.GetHeader("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") || auth[len("Bearer "):] != s.cfg.CallbackToken {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
	}

	var msg models.CallbackMessage
	if err := c.ShouldBindJSON(&msg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	logger := s.logger.With(
		zap.String("type", string(msg.Type)),
		zap.String("txid", msg.TxID),
		zap.String("blockHash", msg.BlockHash),
	)

	switch msg.Type {
	case models.CallbackSeenOnNetwork:
		s.handleSeenOnNetwork(c, msg, logger)
	case models.CallbackSeenMultipleNodes:
		// Just publish event, no state change needed
		logger.Debug("seen by multiple nodes")
	case models.CallbackStump:
		s.handleStump(c, msg, logger)
	case models.CallbackBlockProcessed:
		s.handleBlockProcessed(msg, logger)
	default:
		logger.Warn("unknown callback type")
	}

	c.Status(http.StatusOK)
}

func (s *Server) handleSeenOnNetwork(c *gin.Context, msg models.CallbackMessage, logger *zap.Logger) {
	txids := msg.ResolveSeenTxIDs()
	if len(txids) == 0 {
		return
	}

	ctx := c.Request.Context()
	now := time.Now()
	for _, txid := range txids {
		status := &models.TransactionStatus{
			TxID:      txid,
			Status:    models.StatusSeenOnNetwork,
			Timestamp: now,
		}
		if err := s.store.UpdateStatus(ctx, status); err != nil {
			logger.Warn("failed to update seen_on_network", zap.String("txid", txid), zap.Error(err))
			continue
		}
		if s.txTracker != nil {
			s.txTracker.UpdateStatus(txid, models.StatusSeenOnNetwork)
		}
	}
}

func (s *Server) handleStump(c *gin.Context, msg models.CallbackMessage, logger *zap.Logger) {
	if msg.BlockHash == "" || len(msg.Stump) == 0 {
		logger.Warn("incomplete STUMP callback")
		return
	}

	ctx := c.Request.Context()

	// Extract level-0 hashes from STUMP to discover tracked transactions
	level0Hashes := bump.ExtractLevel0Hashes(msg.Stump)
	if s.txTracker != nil && len(level0Hashes) > 0 {
		tracked := s.txTracker.FilterTrackedHashes(level0Hashes)
		now := time.Now()
		for _, hash := range tracked {
			txid := hash.String()
			status := &models.TransactionStatus{
				TxID:      txid,
				Status:    models.StatusStumpProcessing,
				BlockHash: msg.BlockHash,
				Timestamp: now,
			}
			s.store.UpdateStatus(ctx, status)
			s.txTracker.UpdateStatus(txid, models.StatusStumpProcessing)
		}
	}

	// Store STUMP keyed by (blockHash, subtreeIndex)
	stump := &models.Stump{
		BlockHash:    msg.BlockHash,
		SubtreeIndex: msg.SubtreeIndex,
		StumpData:    msg.Stump,
	}
	if err := s.store.InsertStump(ctx, stump); err != nil {
		logger.Error("failed to store STUMP", zap.Error(err))
	}

	// Publish to Kafka for downstream processing
	s.producer.Send(kafka.TopicStump, msg.BlockHash, msg)
}

func (s *Server) handleBlockProcessed(msg models.CallbackMessage, logger *zap.Logger) {
	if msg.BlockHash == "" {
		return
	}
	if err := s.producer.Send(kafka.TopicBlockProcessed, msg.BlockHash, msg); err != nil {
		logger.Error("failed to publish block_processed", zap.Error(err))
	}
}

// handleGetTransaction retrieves a transaction status by TXID.
func (s *Server) handleGetTransaction(c *gin.Context) {
	txid := c.Param("txid")
	if txid == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "txid is required"})
		return
	}

	status, err := s.store.GetStatus(c.Request.Context(), txid)
	if err != nil {
		s.logger.Error("failed to get status", zap.String("txid", txid), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	if status == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "transaction not found"})
		return
	}

	c.JSON(http.StatusOK, status)
}

// handleSubmitTransaction accepts transactions for validation and propagation.
// Supports application/octet-stream, text/plain (hex), and JSON.
func (s *Server) handleSubmitTransaction(c *gin.Context) {
	var rawTx []byte

	contentType := c.ContentType()
	switch {
	case strings.Contains(contentType, "octet-stream"):
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read body"})
			return
		}
		rawTx = body
	case strings.Contains(contentType, "text/plain"):
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read body"})
			return
		}
		decoded, err := hex.DecodeString(strings.TrimSpace(string(body)))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid hex"})
			return
		}
		rawTx = decoded
	default:
		// JSON format
		var req struct {
			RawTx string `json:"rawTx"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
			return
		}
		decoded, err := hex.DecodeString(req.RawTx)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid hex in rawTx"})
			return
		}
		rawTx = decoded
	}

	if len(rawTx) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "empty transaction"})
		return
	}

	// Publish to transaction topic for validation
	msg := map[string]interface{}{
		"action": "submit",
		"raw_tx": hex.EncodeToString(rawTx),
	}
	if err := s.producer.Send(kafka.TopicTransaction, "", msg); err != nil {
		s.logger.Error("failed to publish transaction", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to submit"})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{"status": "submitted"})
}
