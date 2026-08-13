package v1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/duynhlab/inventory-service/internal/core/domain"
	"github.com/duynhlab/pkg/authmw"
)

// fakeLogic scripts the adminLogic port per test.
type fakeLogic struct {
	balances     []domain.BalanceView
	balanceTotal int
	balanceFilt  domain.BalanceFilter
	skuItems     []domain.BalanceView
	movements    []domain.MovementView
	moveTotal    int
	reservations []domain.ReservationView
	resvTotal    int
	resvStatus   string
	limit        int
	offset       int

	gotCmd  domain.StockCommand
	applied bool
	err     error
}

func (f *fakeLogic) ListBalances(_ context.Context, filter domain.BalanceFilter, limit, offset int) ([]domain.BalanceView, int, error) {
	f.balanceFilt, f.limit, f.offset = filter, limit, offset
	return f.balances, f.balanceTotal, f.err
}

func (f *fakeLogic) SKUBalances(_ context.Context, _ string) ([]domain.BalanceView, error) {
	return f.skuItems, f.err
}

func (f *fakeLogic) ListMovements(_ context.Context, _ domain.MovementFilter, limit, offset int) ([]domain.MovementView, int, error) {
	f.limit, f.offset = limit, offset
	return f.movements, f.moveTotal, f.err
}

func (f *fakeLogic) ListReservations(_ context.Context, status string, limit, offset int) ([]domain.ReservationView, int, error) {
	f.resvStatus, f.limit, f.offset = status, limit, offset
	return f.reservations, f.resvTotal, f.err
}

func (f *fakeLogic) ReceiveStock(_ context.Context, cmd domain.StockCommand) (bool, error) {
	f.gotCmd = cmd
	return f.applied, f.err
}

func (f *fakeLogic) AdjustOnHand(_ context.Context, cmd domain.StockCommand) (bool, error) {
	f.gotCmd = cmd
	return f.applied, f.err
}

// fakeAuth mimics authmw.MiddlewareJWT success: it sets the verified subject
// and roles the way the real middleware does, so the REAL role gate
// (authmw.MiddlewareRequireRole) runs in every test.
func fakeAuth(sub string, roles ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(authmw.CtxUserID, sub)
		c.Set(authmw.CtxRoles, roles)
		c.Next()
	}
}

func setup(t *testing.T, logic *fakeLogic, mw ...gin.HandlerFunc) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewHandler(logic).mount(r, mw...)
	return r
}

func operatorRouter(t *testing.T, logic *fakeLogic) *gin.Engine {
	return setup(t, logic,
		fakeAuth("a11ce000-0000-4000-8000-000000000001", "customer", backofficeRole),
		authmw.MiddlewareRequireRole(backofficeRole))
}

func do(r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestRoleGate(t *testing.T) {
	// A customer token without backoffice_admin is stopped by the real role
	// gate on EVERY protected route with the platform 403 envelope.
	logic := &fakeLogic{}
	r := setup(t, logic,
		fakeAuth("b0b00000-0000-4000-8000-000000000002", "customer"),
		authmw.MiddlewareRequireRole(backofficeRole))

	routes := []struct{ method, path, body string }{
		{http.MethodGet, "/inventory/v1/protected/balances", ""},
		{http.MethodGet, "/inventory/v1/protected/balances/SKU-1", ""},
		{http.MethodGet, "/inventory/v1/protected/movements", ""},
		{http.MethodGet, "/inventory/v1/protected/reservations", ""},
		{http.MethodPost, "/inventory/v1/protected/receipts", `{}`},
		{http.MethodPost, "/inventory/v1/protected/adjustments", `{}`},
	}
	for _, rt := range routes {
		w := do(r, rt.method, rt.path, rt.body)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s %s: want 403, got %d", rt.method, rt.path, w.Code)
		}
		var envelope struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil || envelope.Code != "FORBIDDEN" {
			t.Fatalf("%s %s: want FORBIDDEN envelope, got %s", rt.method, rt.path, w.Body.String())
		}
	}
}

func TestListBalances(t *testing.T) {
	logic := &fakeLogic{
		balances: []domain.BalanceView{{
			SKUID: "SKU-1", WarehouseID: 1, OnHand: 10, Reserved: 4,
			SafetyStock: 2, ATP: 6, UpdatedAt: "2026-08-13T00:00:00Z",
		}},
		balanceTotal: 41,
	}
	r := operatorRouter(t, logic)

	w := do(r, http.MethodGet, "/inventory/v1/protected/balances?page=2&page_size=20&sku_id=SKU-1&warehouse_id=1&low_stock=true", "")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	want := domain.BalanceFilter{SKUID: "SKU-1", WarehouseID: 1, LowStockOnly: true}
	if logic.balanceFilt != want {
		t.Fatalf("filter: want %+v, got %+v", want, logic.balanceFilt)
	}
	if logic.limit != 20 || logic.offset != 20 {
		t.Fatalf("paging: want limit 20 offset 20, got %d/%d", logic.limit, logic.offset)
	}
	var resp struct {
		Items      []map[string]any `json:"items"`
		TotalItems int              `json:"total_items"`
		TotalPages int              `json:"total_pages"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.TotalItems != 41 || resp.TotalPages != 3 {
		t.Fatalf("want totals 41/3, got %d/%d", resp.TotalItems, resp.TotalPages)
	}
	if resp.Items[0]["atp"] != float64(6) {
		t.Fatalf("want derived atp 6 in payload, got %v", resp.Items[0]["atp"])
	}
}

func TestListBalancesRejectsBadWarehouse(t *testing.T) {
	r := operatorRouter(t, &fakeLogic{})
	w := do(r, http.MethodGet, "/inventory/v1/protected/balances?warehouse_id=abc", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestSKUBalancesNotFound(t *testing.T) {
	// An untracked SKU (no balance rows) is a 404, not an empty 200 — the
	// operator must not read "SKU exists with zero stock" into a typo.
	r := operatorRouter(t, &fakeLogic{skuItems: []domain.BalanceView{}})
	w := do(r, http.MethodGet, "/inventory/v1/protected/balances/NOPE", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestListReservationsValidatesStatus(t *testing.T) {
	logic := &fakeLogic{}
	r := operatorRouter(t, logic)

	if w := do(r, http.MethodGet, "/inventory/v1/protected/reservations?status=bogus", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("bogus status: want 400, got %d", w.Code)
	}
	if w := do(r, http.MethodGet, "/inventory/v1/protected/reservations?status=released", ""); w.Code != http.StatusOK {
		t.Fatalf("valid status: want 200, got %d", w.Code)
	}
	if logic.resvStatus != "released" {
		t.Fatalf("status filter not forwarded, got %q", logic.resvStatus)
	}
}

func TestReceiveStock(t *testing.T) {
	logic := &fakeLogic{applied: true}
	r := operatorRouter(t, logic)

	w := do(r, http.MethodPost, "/inventory/v1/protected/receipts",
		`{"command_id":"rcpt-1","sku_id":"SKU-1","warehouse_id":1,"quantity":5,"reason":"PO-77"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body.String())
	}
	// The actor is the verified token subject — never a body field.
	if logic.gotCmd.Actor != "a11ce000-0000-4000-8000-000000000001" {
		t.Fatalf("actor: want token sub, got %q", logic.gotCmd.Actor)
	}
	if logic.gotCmd.Quantity != 5 || logic.gotCmd.CommandID != "rcpt-1" {
		t.Fatalf("command not forwarded: %+v", logic.gotCmd)
	}
}

func TestReceiveStockReplayIs200(t *testing.T) {
	r := operatorRouter(t, &fakeLogic{applied: false})
	w := do(r, http.MethodPost, "/inventory/v1/protected/receipts",
		`{"command_id":"rcpt-1","sku_id":"SKU-1","warehouse_id":1,"quantity":5}`)
	if w.Code != http.StatusOK {
		t.Fatalf("replay: want 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"applied":false`) {
		t.Fatalf("replay: want applied:false, got %s", w.Body.String())
	}
}

func TestReceiveStockValidation(t *testing.T) {
	r := operatorRouter(t, &fakeLogic{})
	cases := []string{
		`{"sku_id":"SKU-1","warehouse_id":1,"quantity":5}`,               // no command_id
		`{"command_id":"c","sku_id":"SKU-1","warehouse_id":1}`,           // no quantity
		`{"command_id":"c","sku_id":"S","warehouse_id":1,"quantity":-2}`, // negative
	}
	for _, body := range cases {
		if w := do(r, http.MethodPost, "/inventory/v1/protected/receipts", body); w.Code != http.StatusBadRequest {
			t.Fatalf("body %s: want 400, got %d", body, w.Code)
		}
	}
}

func TestAdjustmentRequiresReason(t *testing.T) {
	r := operatorRouter(t, &fakeLogic{})
	w := do(r, http.MethodPost, "/inventory/v1/protected/adjustments",
		`{"command_id":"adj-1","sku_id":"SKU-1","warehouse_id":1,"delta":-2}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing reason: want 400, got %d", w.Code)
	}
}

func TestAdjustmentErrorMapping(t *testing.T) {
	body := `{"command_id":"adj-1","sku_id":"SKU-1","warehouse_id":1,"delta":-9,"reason":"shrinkage"}`
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{domain.ErrInsufficientOnHand, http.StatusConflict, "STOCK_UNAVAILABLE"},
		{domain.ErrCommandConflict, http.StatusConflict, "IDEMPOTENCY_CONFLICT"},
		{fmt.Errorf("%w: sku_id must not be empty", domain.ErrInvalidCommand), http.StatusBadRequest, "VALIDATION_ERROR"},
		{errors.New("pg down"), http.StatusInternalServerError, "INTERNAL_ERROR"},
	}
	for _, tc := range cases {
		r := operatorRouter(t, &fakeLogic{err: tc.err})
		w := do(r, http.MethodPost, "/inventory/v1/protected/adjustments", body)
		if w.Code != tc.status {
			t.Fatalf("%v: want %d, got %d", tc.err, tc.status, w.Code)
		}
		var envelope struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &envelope)
		if envelope.Code != tc.code {
			t.Fatalf("%v: want code %s, got %s", tc.err, tc.code, envelope.Code)
		}
	}
}

func TestInternalErrorLeaksNothing(t *testing.T) {
	r := operatorRouter(t, &fakeLogic{err: errors.New("dial tcp 10.0.0.7:5432: connect refused")})
	w := do(r, http.MethodGet, "/inventory/v1/protected/balances", "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "5432") {
		t.Fatalf("internal detail leaked: %s", w.Body.String())
	}
}

func TestListMovements(t *testing.T) {
	logic := &fakeLogic{
		movements: []domain.MovementView{{
			ID: 9, CommandID: "c1", SKUID: "SKU-1", WarehouseID: 1, Type: "RECEIVE",
			OnHandDelta: 7, Reason: "PO-1", Actor: "a11ce000-0000-4000-8000-000000000001",
			CreatedAt: "2026-08-13T00:00:00Z",
		}},
		moveTotal: 1,
	}
	r := operatorRouter(t, logic)

	w := do(r, http.MethodGet, "/inventory/v1/protected/movements?page=1&page_size=10&sku_id=SKU-1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if logic.limit != 10 || logic.offset != 0 {
		t.Fatalf("paging: want limit 10 offset 0, got %d/%d", logic.limit, logic.offset)
	}
	body := w.Body.String()
	for _, want := range []string{`"type":"RECEIVE"`, `"actor":"a11ce000-0000-4000-8000-000000000001"`, `"on_hand_delta":7`} {
		if !strings.Contains(body, want) {
			t.Fatalf("payload missing %s: %s", want, body)
		}
	}

	if w := do(r, http.MethodGet, "/inventory/v1/protected/movements?warehouse_id=zero", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("bad warehouse: want 400, got %d", w.Code)
	}
}

func TestSKUBalancesFound(t *testing.T) {
	logic := &fakeLogic{skuItems: []domain.BalanceView{{SKUID: "SKU-1", WarehouseID: 1, ATP: 5}}}
	r := operatorRouter(t, logic)
	w := do(r, http.MethodGet, "/inventory/v1/protected/balances/SKU-1", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"atp":5`) {
		t.Fatalf("want 200 with atp, got %d: %s", w.Code, w.Body.String())
	}
}

func TestMissingSubjectFailsClosed(t *testing.T) {
	// The auth middleware sets roles but no subject — a wiring bug must be a
	// 401, never a command with an empty actor.
	logic := &fakeLogic{applied: true}
	r := setup(t, logic,
		func(c *gin.Context) { c.Set(authmw.CtxRoles, []string{backofficeRole}); c.Next() },
		authmw.MiddlewareRequireRole(backofficeRole))
	w := do(r, http.MethodPost, "/inventory/v1/protected/receipts",
		`{"command_id":"c","sku_id":"S","warehouse_id":1,"quantity":1}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
	if logic.gotCmd.CommandID != "" {
		t.Fatalf("logic must not be called without a verified subject")
	}
}

func TestRegisterRoutesWiresTheRealChain(t *testing.T) {
	// The production wiring: a real verifier + the real role gate. No token on
	// the wire, so the JWT middleware itself must reject with 401.
	verifier, err := authmw.NewVerifier(authmw.Config{
		Issuer:   "http://localhost:8081/realms/duynhlab",
		Audience: "duynhlab-platform",
	})
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterRoutes(r, NewHandler(&fakeLogic{}), verifier)
	w := do(r, http.MethodGet, "/inventory/v1/protected/balances", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("tokenless: want 401 from the real chain, got %d", w.Code)
	}
}
