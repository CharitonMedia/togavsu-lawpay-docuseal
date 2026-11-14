package lawpaydocuseal

import (
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/CharitonMedia/togavsu-lawpay-docuseal/adapters"
)

//go:embed web/openapi.json
var openapiJSON []byte

// OpenAPI returns the module's own API description (served at /m/lawpay-docuseal/api/openapi.json).
func OpenAPI() []byte { return openapiJSON }

func decode(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fail(http.StatusBadRequest, "bad_request", "The request body isn't valid JSON.")
	}
	return nil
}

// apiFunc is one agent API operation: the verified viewer and the request.
type apiFunc func(ctx context.Context, v viewer, r *http.Request) (int, any, error)

func (s *Server) api(f apiFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := s.viewer(r)
		if err != nil {
			s.writeErr(w, err)
			return
		}
		status, out, err := f(r.Context(), v, r)
		if err != nil {
			s.writeErr(w, err)
			return
		}
		if out == nil {
			w.WriteHeader(status)
			return
		}
		writeJSON(w, status, out)
	}
}

func (s *Server) apiRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/openapi.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(openapiJSON)
	})
	m.HandleFunc("POST /api/pay-intents", s.api(func(ctx context.Context, v viewer, r *http.Request) (int, any, error) {
		var req PayRequest
		if err := decode(r, &req); err != nil {
			return 0, nil, err
		}
		in, err := s.CreateIntent(ctx, v, req)
		return http.StatusCreated, in, err
	}))
	m.HandleFunc("POST /api/pay-intents/{id}/submit", s.api(func(ctx context.Context, v viewer, r *http.Request) (int, any, error) {
		var req struct {
			Token string `json:"token"`
		}
		if err := decode(r, &req); err != nil {
			return 0, nil, err
		}
		if req.Token == "" {
			return 0, nil, fail(http.StatusUnprocessableEntity, "validation_failed", "Enter the card or bank details.")
		}
		res, err := s.Submit(ctx, v, r.PathValue("id"), req.Token)
		return http.StatusOK, res, err
	}))
	m.HandleFunc("POST /api/refunds", s.api(func(ctx context.Context, v viewer, r *http.Request) (int, any, error) {
		var req RefundRequest
		if err := decode(r, &req); err != nil {
			return 0, nil, err
		}
		res, err := s.Refund(ctx, v, req)
		return http.StatusOK, res, err
	}))
	m.HandleFunc("GET /api/refunds/preview", s.api(func(ctx context.Context, v viewer, r *http.Request) (int, any, error) {
		if !v.staff() {
			return 0, nil, errNotFound
		}
		amount, err := adapters.ParseCents(r.URL.Query().Get("amount_cents"))
		if err != nil {
			return 0, nil, fail(http.StatusUnprocessableEntity, "validation_failed", "Enter the amount in cents.")
		}
		p, _, left, err := s.refundable(ctx, v.auth(), r.URL.Query().Get("payment_id"))
		if err != nil {
			return 0, nil, err
		}
		return http.StatusOK, map[string]any{"remaining_cents": left, "warning": s.TrustWarning(ctx, v, p, amount), "can_refund": v.canRefund()}, nil
	}))
	m.HandleFunc("POST /api/refunds/{id}/issue", s.api(func(ctx context.Context, v viewer, r *http.Request) (int, any, error) {
		res, err := s.IssueProposed(ctx, v, r.PathValue("id"))
		return http.StatusOK, res, err
	}))
	m.HandleFunc("POST /api/refunds/{id}/dismiss", s.api(func(ctx context.Context, v viewer, r *http.Request) (int, any, error) {
		return http.StatusNoContent, nil, s.DismissProposed(ctx, v, r.PathValue("id"))
	}))
	m.HandleFunc("GET /api/transactions", s.api(func(ctx context.Context, v viewer, r *http.Request) (int, any, error) {
		ts, err := s.Transactions(ctx, v, r.URL.Query().Get("state"), r.URL.Query().Get("kind"))
		return http.StatusOK, map[string]any{"data": ts}, err
	}))
	m.HandleFunc("GET /api/exceptions", s.api(func(ctx context.Context, v viewer, r *http.Request) (int, any, error) {
		es, err := s.Exceptions(ctx, v, r.URL.Query().Get("kind"), r.URL.Query().Get("status"))
		return http.StatusOK, map[string]any{"data": es}, err
	}))
	m.HandleFunc("POST /api/exceptions/{id}/resolve", s.api(func(ctx context.Context, v viewer, r *http.Request) (int, any, error) {
		var req struct {
			Note string `json:"note"`
		}
		if err := decode(r, &req); err != nil {
			return 0, nil, err
		}
		return http.StatusNoContent, nil, s.Resolve(ctx, v, r.PathValue("id"), req.Note)
	}))
	m.HandleFunc("POST /api/exceptions/{id}/record", s.api(func(ctx context.Context, v viewer, r *http.Request) (int, any, error) {
		var req struct {
			MatterID  string `json:"matter_id"`
			ContactID string `json:"contact_id"`
		}
		if err := decode(r, &req); err != nil {
			return 0, nil, err
		}
		res, err := s.RecordUnknown(ctx, v, r.PathValue("id"), req.MatterID, req.ContactID)
		return http.StatusOK, res, err
	}))
	m.HandleFunc("POST /api/exceptions/{id}/post-refund", s.api(func(ctx context.Context, v viewer, r *http.Request) (int, any, error) {
		var req struct {
			Note string `json:"note"`
		}
		if err := decode(r, &req); err != nil {
			return 0, nil, err
		}
		res, err := s.PostExternal(ctx, v, r.PathValue("id"), req.Note)
		return http.StatusOK, res, err
	}))
	m.HandleFunc("POST /api/invoices/{id}/reopen", s.api(func(ctx context.Context, v viewer, r *http.Request) (int, any, error) {
		return http.StatusNoContent, nil, s.Reopen(ctx, v, r.PathValue("id"))
	}))
	m.HandleFunc("POST /api/matters/{id}/deposits", s.api(func(ctx context.Context, v viewer, r *http.Request) (int, any, error) {
		var req struct {
			Enabled bool `json:"enabled"`
		}
		if err := decode(r, &req); err != nil {
			return 0, nil, err
		}
		return http.StatusNoContent, nil, s.SetDeposits(ctx, v, r.PathValue("id"), req.Enabled)
	}))
	m.HandleFunc("GET /api/agreements", s.api(func(ctx context.Context, v viewer, r *http.Request) (int, any, error) {
		gs, err := s.Agreements(ctx, v, r.URL.Query().Get("matter_id"), r.URL.Query().Get("state"))
		return http.StatusOK, map[string]any{"data": gs}, err
	}))
	m.HandleFunc("POST /api/agreements", s.api(func(ctx context.Context, v viewer, r *http.Request) (int, any, error) {
		var req AgreementRequest
		if err := decode(r, &req); err != nil {
			return 0, nil, err
		}
		a, err := s.SaveDraft(ctx, v, req)
		return http.StatusOK, a, err
	}))
	m.HandleFunc("POST /api/agreements/{id}/send", s.api(func(ctx context.Context, v viewer, r *http.Request) (int, any, error) {
		g, err := s.Send(ctx, v, r.PathValue("id"))
		return http.StatusOK, g, err
	}))
	m.HandleFunc("GET /api/signings/{id}", s.api(func(ctx context.Context, v viewer, r *http.Request) (int, any, error) {
		if !v.staff() {
			return 0, nil, errNotFound
		}
		g, found, err := s.signingBy(ctx, "id", r.PathValue("id"))
		if err == nil && (!found || !s.canSee(ctx, v, g.MatterID)) {
			err = errNotFound
		}
		return http.StatusOK, g, err
	}))
	m.HandleFunc("POST /api/signings/{id}/resend", s.api(func(ctx context.Context, v viewer, r *http.Request) (int, any, error) {
		g, err := s.Resend(ctx, v, r.PathValue("id"))
		return http.StatusOK, g, err
	}))
	m.HandleFunc("POST /api/signings/{id}/void", s.api(func(ctx context.Context, v viewer, r *http.Request) (int, any, error) {
		var req struct {
			Reason string `json:"reason"`
		}
		if err := decode(r, &req); err != nil {
			return 0, nil, err
		}
		g, err := s.Void(ctx, v, r.PathValue("id"), req.Reason)
		return http.StatusOK, g, err
	}))
	m.HandleFunc("GET /api/widgets/awaiting-signature", s.api(func(ctx context.Context, v viewer, r *http.Request) (int, any, error) {
		n, err := s.AwaitingCount(ctx, v)
		return http.StatusOK, map[string]any{"counts": n, "total": n["sent"] + n["client_signed"]}, err
	}))
	m.HandleFunc("POST /api/setup/test", s.api(func(ctx context.Context, v viewer, r *http.Request) (int, any, error) {
		res, err := s.TestConnection(ctx, v)
		return http.StatusOK, res, err
	}))
}

// Agreements lists signings (with their agreements) the viewer can see.
func (s *Server) Agreements(ctx context.Context, v viewer, matter, state string) ([]Signing, error) {
	if !v.staff() {
		return nil, errNotFound
	}
	where, args := `true`, []any{}
	if matter != "" {
		where, args = `matter_id = $1`, append(args, matter)
	}
	if state != "" {
		args = append(args, state)
		where += ` AND state = $` + strconv.Itoa(len(args))
	}
	gs, err := s.signings(ctx, where, args...)
	if err != nil {
		return nil, err
	}
	vis := s.visible(v)
	out := []Signing{}
	for _, g := range gs {
		if vis.ok(ctx, g.MatterID) {
			out = append(out, g)
		}
	}
	return out, nil
}

// TestResult is the setup tab's connection check.
type TestResult struct {
	LawPay   string `json:"lawpay"`
	Trust    string `json:"trust"`
	DocuSeal string `json:"docuseal"`
}

// TestConnection checks the settings: LawPay answers a read with the secret key, the core
// trust account is active, and DocuSeal reads the template. Admins only.
func (s *Server) TestConnection(ctx context.Context, v viewer) (TestResult, error) {
	if !v.Has("admin") {
		return TestResult{}, errForbidden
	}
	st, err := s.RefreshState(ctx)
	if err != nil {
		return TestResult{}, err
	}
	res := TestResult{LawPay: "not set up", Trust: "not set up", DocuSeal: "not set up"}
	if st.LawPayReady() {
		// A read of a transaction that can't exist: 404 means the key works.
		if _, err := s.lawpay(st).Transaction(ctx, "togavsu-connection-check"); err == nil || adapters.NotFound(err) {
			res.LawPay = "ok"
		} else {
			res.LawPay = "failed: " + lawpayMessage(err)
		}
	}
	if st.TrustConfigured() {
		res.Trust = "the Togavsu trust account isn't active"
		if s.trustReady(ctx) {
			res.Trust = "ok"
		}
	}
	if st.DocuSealReady() {
		if err := s.docuseal(st).Template(ctx, st.TemplateID); err == nil {
			res.DocuSeal = "ok"
		} else {
			res.DocuSeal = "failed: " + docusealMessage(err)
		}
	}
	return res, nil
}
