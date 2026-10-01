package gateway

import (
	"github.com/gin-gonic/gin"
)

// RegisterRoutes mounts the health probe and an allow-list of OpenAI-compatible
// routes. Anything else is 404 and never reaches an upstream with our key.
func RegisterRoutes(rg *gin.RouterGroup, h *Handler) {
	rg.GET("/healthz", h.Healthz)
	rg.GET("/readyz", h.Readyz)
	v1 := rg.Group("/v1", h.authenticate, h.logUsage, h.admit)
	v1.POST("/chat/completions", h.prepareChat, h.forward(h.chatProxy))
	v1.POST("/completions", h.prepareChat, h.forward(h.chatProxy))
	v1.POST("/embeddings", h.prepareEmbedding, h.forward(h.embedProxy))
	v1.GET("/models", h.forward(h.chatProxy))
}
