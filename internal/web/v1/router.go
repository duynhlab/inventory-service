// Package v1 holds the inventory service web layer (Gin handlers + routing) —
// the service's first HTTP business surface (RFC-0023 slice A). It translates
// HTTP to the logic layer's AdminService and maps error sentinels to the
// shared httpx envelope; no business rules live here.
//
// Every route is protected: valid realm token (authmw, authoritative) plus
// the backoffice_admin role (ADR-047). The edge performs the same JWT check
// coarsely; this layer never trusts it.
package v1

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/duynhlab/inventory-service/internal/core/domain"
	"github.com/duynhlab/pkg/authmw"
	"github.com/duynhlab/pkg/httpx"
)

// backofficeRole is the realm role every protected route requires (RFC-0022).
const backofficeRole = "backoffice_admin"

// adminLogic is the slice of the logic layer the web layer calls.
// *logicv1.AdminService satisfies it; kept as an interface so handlers are
// testable without a database.
type adminLogic interface {
	ListBalances(ctx context.Context, f domain.BalanceFilter, limit, offset int) ([]domain.BalanceView, int, error)
	SKUBalances(ctx context.Context, skuID string) ([]domain.BalanceView, error)
	ListMovements(ctx context.Context, f domain.MovementFilter, limit, offset int) ([]domain.MovementView, int, error)
	ListReservations(ctx context.Context, status string, limit, offset int) ([]domain.ReservationView, int, error)
	ReceiveStock(ctx context.Context, cmd domain.StockCommand) (bool, error)
	AdjustOnHand(ctx context.Context, cmd domain.StockCommand) (bool, error)
}

// Handler is the inventory web-layer handler.
type Handler struct {
	logic adminLogic
}

// NewHandler creates an inventory handler with dependency injection.
func NewHandler(logic adminLogic) *Handler {
	return &Handler{logic: logic}
}

// RegisterRoutes mounts the protected inventory v1 routes on the engine.
func RegisterRoutes(r *gin.Engine, h *Handler, verifier *authmw.Verifier) {
	h.mount(r, authmw.MiddlewareJWT(verifier), authmw.MiddlewareRequireRole(backofficeRole))
}

// mount registers the routes with the given auth middlewares. Split from
// RegisterRoutes so tests can inject fakes.
func (h *Handler) mount(r *gin.Engine, authMW ...gin.HandlerFunc) {
	protected := r.Group("/inventory/v1/protected")
	protected.Use(authMW...)
	{
		protected.GET("/balances", h.ListBalances)
		protected.GET("/balances/:skuId", h.SKUBalances)
		protected.GET("/movements", h.ListMovements)
		protected.GET("/reservations", h.ListReservations)
		protected.POST("/receipts", h.ReceiveStock)
		protected.POST("/adjustments", h.AdjustOnHand)
	}
}

// actorSub reads the verified token subject set by authmw. The middleware
// chain guarantees it; an absent value is a wiring bug and fails closed.
func actorSub(c *gin.Context) (string, bool) {
	sub := c.GetString(authmw.CtxUserID)
	if sub == "" {
		httpx.RespondError(c, http.StatusUnauthorized, httpx.CodeUnauthorized,
			"Authentication required")
		return "", false
	}
	return sub, true
}

// balanceJSON is the wire shape of one balance row.
type balanceJSON struct {
	SKUID       string `json:"sku_id"`
	WarehouseID int64  `json:"warehouse_id"`
	OnHand      int64  `json:"on_hand"`
	Reserved    int64  `json:"reserved"`
	SafetyStock int64  `json:"safety_stock"`
	ATP         int64  `json:"atp"`
	UpdatedAt   string `json:"updated_at"`
}

func toBalanceJSON(items []domain.BalanceView) []balanceJSON {
	out := make([]balanceJSON, 0, len(items))
	for _, b := range items {
		out = append(out, balanceJSON(b))
	}
	return out
}

// ListBalances serves GET /balances?sku_id=&warehouse_id=&low_stock=&page=&page_size=.
func (h *Handler) ListBalances(c *gin.Context) {
	page, pageSize := httpx.ParsePage(c)

	filter := domain.BalanceFilter{
		SKUID:        c.Query("sku_id"),
		LowStockOnly: c.Query("low_stock") == "true",
	}
	if raw := c.Query("warehouse_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			httpx.RespondError(c, http.StatusBadRequest, httpx.CodeValidation,
				"warehouse_id must be a positive integer")
			return
		}
		filter.WarehouseID = id
	}

	items, total, err := h.logic.ListBalances(c.Request.Context(), filter, pageSize, httpx.Offset(page, pageSize))
	if err != nil {
		respondAdminError(c, err)
		return
	}
	c.JSON(http.StatusOK, httpx.NewPaginated(toBalanceJSON(items), page, pageSize, total))
}

// SKUBalances serves GET /balances/:skuId — all warehouses for one SKU.
func (h *Handler) SKUBalances(c *gin.Context) {
	items, err := h.logic.SKUBalances(c.Request.Context(), c.Param("skuId"))
	if err != nil {
		respondAdminError(c, err)
		return
	}
	if len(items) == 0 {
		httpx.RespondError(c, http.StatusNotFound, httpx.CodeNotFound,
			"SKU has no balance row in any warehouse")
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": toBalanceJSON(items)})
}

// movementJSON is the wire shape of one ledger row.
type movementJSON struct {
	ID            int64  `json:"id"`
	CommandID     string `json:"command_id"`
	SKUID         string `json:"sku_id"`
	WarehouseID   int64  `json:"warehouse_id"`
	Type          string `json:"type"`
	OnHandDelta   int64  `json:"on_hand_delta"`
	ReservedDelta int64  `json:"reserved_delta"`
	ReferenceType string `json:"reference_type"`
	ReferenceID   string `json:"reference_id"`
	Reason        string `json:"reason"`
	Actor         string `json:"actor"`
	CreatedAt     string `json:"created_at"`
}

// ListMovements serves GET /movements?sku_id=&warehouse_id=&page=&page_size=.
func (h *Handler) ListMovements(c *gin.Context) {
	page, pageSize := httpx.ParsePage(c)

	filter := domain.MovementFilter{SKUID: c.Query("sku_id")}
	if raw := c.Query("warehouse_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			httpx.RespondError(c, http.StatusBadRequest, httpx.CodeValidation,
				"warehouse_id must be a positive integer")
			return
		}
		filter.WarehouseID = id
	}

	items, total, err := h.logic.ListMovements(c.Request.Context(), filter, pageSize, httpx.Offset(page, pageSize))
	if err != nil {
		respondAdminError(c, err)
		return
	}
	out := make([]movementJSON, 0, len(items))
	for _, m := range items {
		out = append(out, movementJSON(m))
	}
	c.JSON(http.StatusOK, httpx.NewPaginated(out, page, pageSize, total))
}

// reservationJSON is the wire shape of one reservation header.
type reservationJSON struct {
	ID        string `json:"id"`
	OrderID   string `json:"order_id"`
	Status    string `json:"status"`
	ExpiresAt string `json:"expires_at"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// validReservationStatuses mirrors the schema CHECK constraint.
var validReservationStatuses = map[string]bool{
	"reserved": true, "committed": true, "released": true, "expired": true,
}

// ListReservations serves GET /reservations?status=&page=&page_size=.
func (h *Handler) ListReservations(c *gin.Context) {
	page, pageSize := httpx.ParsePage(c)

	status := c.Query("status")
	if status != "" && !validReservationStatuses[status] {
		httpx.RespondError(c, http.StatusBadRequest, httpx.CodeValidation,
			"status must be one of reserved, committed, released, expired")
		return
	}

	items, total, err := h.logic.ListReservations(c.Request.Context(), status, pageSize, httpx.Offset(page, pageSize))
	if err != nil {
		respondAdminError(c, err)
		return
	}
	out := make([]reservationJSON, 0, len(items))
	for _, v := range items {
		out = append(out, reservationJSON(v))
	}
	c.JSON(http.StatusOK, httpx.NewPaginated(out, page, pageSize, total))
}

// receiptRequest is the POST /receipts body. command_id is the idempotency
// key (inventory's body style — RFC-0023 protected conventions).
type receiptRequest struct {
	CommandID   string `json:"command_id" binding:"required"`
	SKUID       string `json:"sku_id" binding:"required"`
	WarehouseID int64  `json:"warehouse_id" binding:"required"`
	Quantity    int64  `json:"quantity" binding:"required"`
	Reason      string `json:"reason"`
}

// ReceiveStock serves POST /receipts.
func (h *Handler) ReceiveStock(c *gin.Context) {
	sub, ok := actorSub(c)
	if !ok {
		return
	}
	var req receiptRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, httpx.CodeValidation,
			"command_id, sku_id, warehouse_id and quantity are required")
		return
	}
	if req.Quantity <= 0 {
		httpx.RespondError(c, http.StatusBadRequest, httpx.CodeValidation,
			"quantity must be > 0")
		return
	}

	applied, err := h.logic.ReceiveStock(c.Request.Context(), domain.StockCommand{
		CommandID:   req.CommandID,
		SKUID:       req.SKUID,
		WarehouseID: req.WarehouseID,
		Quantity:    req.Quantity,
		Reason:      req.Reason,
		Actor:       sub,
	})
	if err != nil {
		respondAdminError(c, err)
		return
	}
	respondCommand(c, req.CommandID, applied)
}

// adjustmentRequest is the POST /adjustments body. delta is signed; reason is
// mandatory for adjustments (RFC-0023 — every correction carries its why).
type adjustmentRequest struct {
	CommandID   string `json:"command_id" binding:"required"`
	SKUID       string `json:"sku_id" binding:"required"`
	WarehouseID int64  `json:"warehouse_id" binding:"required"`
	Delta       int64  `json:"delta" binding:"required"`
	Reason      string `json:"reason" binding:"required"`
}

// AdjustOnHand serves POST /adjustments.
func (h *Handler) AdjustOnHand(c *gin.Context) {
	sub, ok := actorSub(c)
	if !ok {
		return
	}
	var req adjustmentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, httpx.CodeValidation,
			"command_id, sku_id, warehouse_id, delta and reason are required")
		return
	}

	applied, err := h.logic.AdjustOnHand(c.Request.Context(), domain.StockCommand{
		CommandID:   req.CommandID,
		SKUID:       req.SKUID,
		WarehouseID: req.WarehouseID,
		Quantity:    req.Delta,
		Reason:      req.Reason,
		Actor:       sub,
	})
	if err != nil {
		respondAdminError(c, err)
		return
	}
	respondCommand(c, req.CommandID, applied)
}

// respondCommand reports a command outcome: 201 for a first application,
// 200 for an idempotent replay of the same command_id.
func respondCommand(c *gin.Context, commandID string, applied bool) {
	status := http.StatusCreated
	if !applied {
		status = http.StatusOK
	}
	c.JSON(status, gin.H{"command_id": commandID, "applied": applied})
}

// respondAdminError maps domain sentinels onto the shared envelope. Anything
// unmapped is a 500 with no internals leaked.
func respondAdminError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, domain.ErrInvalidCommand):
		httpx.RespondError(c, http.StatusBadRequest, httpx.CodeValidation, err.Error())
	case errors.Is(err, domain.ErrCommandConflict):
		httpx.RespondError(c, http.StatusConflict, httpx.CodeIdempotencyConflict,
			"command_id was already used with a different payload")
	case errors.Is(err, domain.ErrInsufficientOnHand):
		httpx.RespondError(c, http.StatusConflict, httpx.CodeStockUnavailable,
			"adjustment would violate balance invariants (on_hand >= reserved >= 0)")
	case errors.Is(err, domain.ErrBalanceNotFound):
		httpx.RespondError(c, http.StatusNotFound, httpx.CodeNotFound,
			"no balance row for that sku and warehouse")
	default:
		httpx.RespondError(c, http.StatusInternalServerError, httpx.CodeInternal,
			"Internal server error")
	}
}
