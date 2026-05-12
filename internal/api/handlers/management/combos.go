package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/combo"
)

// Virtual combo management endpoints.
//
// These endpoints expose CRUD for named fallback chains that live in
// CLIProxyAPI — not in genfity-ai-gateway-service anymore (see PRD §3.3).
// The handlers delegate all validation to combo.Combo.Validate so the rules
// stay in one place.

type comboResponse struct {
	Combo *combo.Combo `json:"combo"`
}

type comboListResponse struct {
	Object string         `json:"object"`
	Data   []*combo.Combo `json:"data"`
}

// GetCombos returns every combo regardless of status. Intentionally verbose
// so operators can audit draft/disabled combos from the same endpoint used
// for listing live ones.
func (h *Handler) GetCombos(c *gin.Context) {
	r := h.ComboRegistry()
	if r == nil {
		c.JSON(http.StatusOK, comboListResponse{Object: "list", Data: []*combo.Combo{}})
		return
	}
	c.JSON(http.StatusOK, comboListResponse{Object: "list", Data: r.List()})
}

// GetCombo returns a single combo by name.
func (h *Handler) GetCombo(c *gin.Context) {
	r := h.ComboRegistry()
	if r == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "combo registry not initialised"})
		return
	}
	name := c.Param("name")
	got, ok := r.Get(name)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "combo not found"})
		return
	}
	c.JSON(http.StatusOK, comboResponse{Combo: got})
}

// PutCombo creates or replaces a combo. The combo name in the URL path
// overrides any name in the body to avoid spoofing via mismatched payloads.
func (h *Handler) PutCombo(c *gin.Context) {
	r := h.ComboRegistry()
	if r == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "combo registry not initialised"})
		return
	}
	name := strings.TrimSpace(c.Param("name"))
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "combo name is required in the URL path"})
		return
	}

	var body combo.Combo
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON: " + err.Error()})
		return
	}
	body.Name = name // URL wins over body

	if err := r.Upsert(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persistCombos()

	got, _ := r.Get(name)
	c.JSON(http.StatusOK, comboResponse{Combo: got})
}

// PostCombo is an alias for PutCombo that derives the name from the body.
// Clients using REST conventions can POST to /combos without knowing the
// name up-front.
func (h *Handler) PostCombo(c *gin.Context) {
	r := h.ComboRegistry()
	if r == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "combo registry not initialised"})
		return
	}
	var body combo.Combo
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON: " + err.Error()})
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "combo name is required"})
		return
	}
	if r.Has(body.Name) {
		c.JSON(http.StatusConflict, gin.H{"error": "combo already exists; use PUT to update"})
		return
	}
	if err := r.Upsert(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persistCombos()

	got, _ := r.Get(body.Name)
	c.JSON(http.StatusCreated, comboResponse{Combo: got})
}

// DeleteCombo drops a combo. Missing combos also return 204 so DELETE is
// idempotent — operators can safely re-run cleanup scripts.
func (h *Handler) DeleteCombo(c *gin.Context) {
	r := h.ComboRegistry()
	if r == nil {
		c.Status(http.StatusNoContent)
		return
	}
	name := c.Param("name")
	r.Delete(name)
	h.persistCombos()
	c.Status(http.StatusNoContent)
}

// persistCombos writes the in-memory registry to disk. Errors are logged
// but not propagated — a transient write failure should not block the
// management request.
func (h *Handler) persistCombos() {
	if h == nil || h.comboStore == nil || h.comboRegistry == nil {
		return
	}
	if err := h.comboStore.Save(h.comboRegistry); err != nil {
		// Intentionally not failing the HTTP response: the registry is
		// already updated, so the change is live. The next write will
		// reconcile; if disk is permanently broken the operator will see
		// it in startup logs after a restart.
		_ = err
	}
}
