package api_server

import "github.com/gin-gonic/gin"

type RouteDoc struct {
	Method         string
	Path           string
	Description    string
	RequestFormat  string
	ResponseFormat string
}

var routeDocs = []RouteDoc{
	{
		Method:         "GET",
		Path:           "/health",
		Description:    "Health check",
		RequestFormat:  "None",
		ResponseFormat: `{"status": "ok"}`,
	},
	{
		Method:         "GET",
		Path:           "/ready",
		Description:    "Readiness check",
		RequestFormat:  "None",
		ResponseFormat: `{"status": "ready"}`,
	},
	{
		Method:         "GET",
		Path:           "/tx/:txid",
		Description:    "Get transaction status by txid",
		RequestFormat:  "Path param: txid",
		ResponseFormat: `{"txid", "txStatus", "status", "timestamp", "blockHash", "blockHeight", "merklePath", "extraInfo", "competingTxs"}`,
	},
	{
		Method:         "POST",
		Path:           "/tx",
		Description:    "Submit a transaction",
		RequestFormat:  "application/octet-stream (raw), text/plain (hex), or JSON {\"rawTx\": \"<hex>\"}",
		ResponseFormat: `{"status": "submitted"} (202 Accepted)`,
	},
	{
		Method:         "POST",
		Path:           "/txs",
		Description:    "Submit a batch of transactions",
		RequestFormat:  "application/octet-stream (concatenated raw tx bytes)",
		ResponseFormat: `{"submitted": N} (200 OK)`,
	},
	{
		Method:         "POST",
		Path:           "/api/v1/merkle-service/callback",
		Description:    "Receive callbacks from Merkle Service",
		RequestFormat:  "JSON CallbackMessage with type field. Optional Bearer token auth.",
		ResponseFormat: "200 OK",
	},
}

func (s *Server) registerRoutes(r *gin.Engine) {
	r.GET("/", s.handleDocs)

	r.GET("/health", s.handleHealth)
	r.GET("/ready", s.handleReady)

	r.POST("/api/v1/merkle-service/callback", s.handleCallback)
	r.GET("/tx/:txid", s.handleGetTransaction)
	r.POST("/tx", s.handleSubmitTransaction)
	r.POST("/txs", s.handleSubmitTransactions)
}
