package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/imagerouting"
)

// Global image-routing management endpoints. A single config object governs
// which combos re-route image-carrying requests and the shared fallback
// chain they route to. Validation lives in imagerouting.Config.Validate so
// the rules stay in one place.

type imageRoutingResponse struct {
	Config *imagerouting.Config `json:"config"`
}

// GetImageRouting returns the current global image-routing config. A disabled
// or never-configured feature returns an empty config (Enabled=false) so the
// UI can render a clean initial state.
func (h *Handler) GetImageRouting(c *gin.Context) {
	r := h.ImageRoutingRegistry()
	if r == nil {
		c.JSON(http.StatusOK, imageRoutingResponse{Config: &imagerouting.Config{}})
		return
	}
	c.JSON(http.StatusOK, imageRoutingResponse{Config: r.Get()})
}

// PutImageRouting replaces the global image-routing config. The whole object
// is validated before it goes live; on success it is applied to the live
// registry immediately and persisted best-effort.
func (h *Handler) PutImageRouting(c *gin.Context) {
	r := h.ImageRoutingRegistry()
	if r == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "image routing registry not initialised"})
		return
	}
	var body imagerouting.Config
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON: " + err.Error()})
		return
	}
	body.Normalize()
	if err := body.Validate(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	r.Set(&body)
	h.persistImageRouting()
	c.JSON(http.StatusOK, imageRoutingResponse{Config: r.Get()})
}

// persistImageRouting writes the live registry to disk. Errors are swallowed
// (not propagated) — the registry is already updated so the change is live;
// the next write reconciles. Mirrors persistCombos.
func (h *Handler) persistImageRouting() {
	if h == nil || h.imageStore == nil || h.imageRegistry == nil {
		return
	}
	_ = h.imageStore.Save(h.imageRegistry)
}
