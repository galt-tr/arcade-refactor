package api_server

import (
	"encoding/hex"
	"html/template"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/bsv-blockchain/arcade/kafka"
	"github.com/bsv-blockchain/arcade/models"
	sdkTx "github.com/bsv-blockchain/go-sdk/transaction"
)

const docsTemplate = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>Arcade API</title>
<style>
  body { font-family: system-ui, sans-serif; max-width: 900px; margin: 40px auto; padding: 0 20px; color: #333; }
  h1 { border-bottom: 2px solid #eee; padding-bottom: 10px; }
  table { width: 100%; border-collapse: collapse; margin-top: 20px; }
  th, td { text-align: left; padding: 10px 12px; border-bottom: 1px solid #eee; }
  th { background: #f8f8f8; font-weight: 600; }
  code { background: #f4f4f4; padding: 2px 6px; border-radius: 3px; font-size: 0.9em; }
  .method { font-weight: bold; }
  .get { color: #2e7d32; }
  .post { color: #1565c0; }
</style>
</head>
<body>
<h1>Arcade API</h1>
<p>Available routes:</p>
<table>
  <tr><th>Method</th><th>Path</th><th>Description</th><th>Request</th><th>Response</th></tr>
  {{range .}}<tr>
    <td class="method {{.Method | lower}}">{{.Method}}</td>
    <td><code>{{.Path}}</code></td>
    <td>{{.Description}}</td>
    <td>{{.RequestFormat}}</td>
    <td><code>{{.ResponseFormat}}</code></td>
  </tr>{{end}}
</table>
</body>
</html>`

var docsTmpl = template.Must(template.New("docs").Funcs(template.FuncMap{
	"lower": strings.ToLower,
}).Parse(docsTemplate))

func (s *Server) handleDocs(c *gin.Context) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Status(http.StatusOK)
	if err := docsTmpl.Execute(c.Writer, routeDocs); err != nil {
		s.logger.Error("failed to render docs", zap.Error(err))
	}
}

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
		zap.Strings("txids", msg.TxIDs),
		zap.String("blockHash", msg.BlockHash),
	)

	switch msg.Type {
	case models.CallbackSeenOnNetwork:
		s.handleSeenOnNetwork(c, msg, logger)
	case models.CallbackSeenMultipleNodes:
		s.handleSeenMultipleNodes(c, msg, logger)
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

func (s *Server) handleSeenMultipleNodes(c *gin.Context, msg models.CallbackMessage, logger *zap.Logger) {
	txids := msg.ResolveSeenTxIDs()
	if len(txids) == 0 {
		return
	}

	ctx := c.Request.Context()
	now := time.Now()
	for _, txid := range txids {
		status := &models.TransactionStatus{
			TxID:      txid,
			Status:    models.StatusSeenMultipleNodes,
			Timestamp: now,
		}
		if err := s.store.UpdateStatus(ctx, status); err != nil {
			logger.Warn("failed to update seen_multiple_nodes", zap.String("txid", txid), zap.Error(err))
			continue
		}
		if s.txTracker != nil {
			s.txTracker.UpdateStatus(txid, models.StatusSeenMultipleNodes)
		}
	}
}

func (s *Server) handleStump(c *gin.Context, msg models.CallbackMessage, logger *zap.Logger) {
	if msg.BlockHash == "" || len(msg.Stump) == 0 {
		logger.Warn("incomplete STUMP callback")
		return
	}

	ctx := c.Request.Context()

	// Store STUMP keyed by (blockHash, subtreeIndex)
	stump := &models.Stump{
		BlockHash:    msg.BlockHash,
		SubtreeIndex: msg.SubtreeIndex,
		StumpData:    msg.Stump,
	}
	if err := s.store.InsertStump(ctx, stump); err != nil {
		logger.Error("failed to store STUMP", zap.Error(err))
		return
	}

	// Publish to Kafka for downstream processing
	if err := s.producer.Send(kafka.TopicStump, msg.BlockHash, msg); err != nil {
		logger.Error("failed to publish STUMP to kafka", zap.Error(err))
	}
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

// handleSubmitTransactions accepts a batch of concatenated raw transactions.
func (s *Server) handleSubmitTransactions(c *gin.Context) {
	if !strings.Contains(c.ContentType(), "octet-stream") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Content-Type must be application/octet-stream"})
		return
	}

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read body"})
		return
	}
	if len(body) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "empty body"})
		return
	}

	// Phase 1: Parse all transactions upfront
	var msgs []kafka.KeyValue
	offset := 0
	for offset < len(body) {
		_, bytesUsed, parseErr := sdkTx.NewTransactionFromStream(body[offset:])
		if parseErr != nil {
			s.logger.Error("failed to parse transaction in batch",
				zap.Int("offset", offset),
				zap.Int("parsed", len(msgs)),
				zap.Error(parseErr),
			)
			c.JSON(http.StatusBadRequest, gin.H{"error": "failed to parse transaction", "parsed": len(msgs)})
			return
		}
		if bytesUsed == 0 {
			break
		}

		rawTxBytes := body[offset : offset+bytesUsed]
		msgs = append(msgs, kafka.KeyValue{
			Key: "",
			Value: map[string]interface{}{
				"action": "submit",
				"raw_tx": hex.EncodeToString(rawTxBytes),
			},
		})
		offset += bytesUsed
	}

	if len(msgs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no transactions parsed"})
		return
	}

	// Phase 2: Batch publish all parsed transactions in one call
	if err := s.producer.SendBatch(kafka.TopicTransaction, msgs); err != nil {
		s.logger.Error("failed to publish transaction batch", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to submit"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"submitted": len(msgs)})
}
