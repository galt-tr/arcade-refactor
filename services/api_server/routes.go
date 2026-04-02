package api_server

import "github.com/gin-gonic/gin"

func (s *Server) registerRoutes(r *gin.Engine) {
	r.GET("/health", s.handleHealth)
	r.GET("/ready", s.handleReady)

	r.POST("/api/v1/merkle-service/callback", s.handleCallback)
	r.GET("/tx/:txid", s.handleGetTransaction)
	r.POST("/tx", s.handleSubmitTransaction)
}
