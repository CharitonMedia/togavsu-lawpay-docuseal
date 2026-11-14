package lawpaydocuseal

import (
	"context"
	_ "embed"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

//go:embed web/pages.html
var pagesHTML string

//go:embed web/hostedfields.js
var hostedFieldsJS []byte

var pages = template.Must(template.New("pages").Funcs(template.FuncMap{
	"dollars": dollars,
	"plain":   plainDollars,
	"deref":   func(p *int64) int64 { return *p },
}).Parse(pagesHTML))

type data map[string]any

// returnTo honors return_to only inside the client portal module; anything else is /portal.
func returnTo(v string) string {
	if strings.HasPrefix(v, "/portal/client-portal/") && !strings.Contains(v, "//") && !strings.Contains(v, "\\") {
		return v
	}
	return "/portal"
}

func (s *Server) render(w http.ResponseWriter, status int, name string, d data) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := pages.ExecuteTemplate(w, name, d); err != nil {
		s.c.Logf("lawpay-docuseal: page %s: %v", name, err)
	}
}

// pageErr shows an error as a page: not set up with the settings link, 404 as not found,
// and anything else as its message.
func (s *Server) pageErr(w http.ResponseWriter, err error, back string) {
	e := *asAppError(err)
	ae := &e
	if ae.Code == "not_configured" {
		s.render(w, http.StatusOK, "notsetup", data{"Title": "Not set up", "Message": ae.Message, "SettingsLink": settingsLink})
		return
	}
	if ae.Status >= http.StatusInternalServerError {
		s.c.Logf("lawpay-docuseal: %v", err)
	}
	title := "Can't do that"
	if ae.Status == http.StatusNotFound {
		title, ae.Message = "Not found", "This page isn't available."
	}
	s.render(w, ae.Status, "message", data{"Title": title, "Message": ae.Message, "Error": true, "Back": back})
}

type pageFunc func(ctx context.Context, v viewer, r *http.Request) (string, data, error)

func (s *Server) page(f pageFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := s.viewer(r)
		if err != nil {
			s.pageErr(w, err, "")
			return
		}
		name, d, err := f(r.Context(), v, r)
		if err != nil {
			back := ""
			if d != nil {
				back, _ = d["Back"].(string)
			}
			s.pageErr(w, err, back)
			return
		}
		d["Context"] = v.Token
		s.render(w, http.StatusOK, name, d)
	}
}

func (s *Server) staffOnly(f pageFunc) pageFunc {
	return func(ctx context.Context, v viewer, r *http.Request) (string, data, error) {
		if !v.staff() {
			return "", nil, errNotFound
		}
		return f(ctx, v, r)
	}
}

func clientOnly(f pageFunc) pageFunc {
	return func(ctx context.Context, v viewer, r *http.Request) (string, data, error) {
		if !v.Client() {
			return "", nil, errNotFound
		}
		return f(ctx, v, r)
	}
}

func matterOf(v viewer, r *http.Request) string {
	if m := v.Matter(); m != "" {
		return m
	}
	if m := r.FormValue("matter_id"); m != "" {
		return m
	}
	return r.URL.Query().Get("matter_id")
}

func (s *Server) pageRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /static/hostedfields.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		_, _ = w.Write(hostedFieldsJS)
	})
	// Client tabs.
	m.HandleFunc("GET /client/pay", s.page(clientOnly(s.clientPay)))
	m.HandleFunc("POST /client/pay", s.page(clientOnly(s.clientPay)))
	m.HandleFunc("POST /client/submit", s.page(clientOnly(s.paySubmit)))
	m.HandleFunc("GET /client/sign", s.page(clientOnly(s.clientSign)))
	// Staff pages.
	m.HandleFunc("GET /matter", s.page(s.staffOnly(s.matterTab)))
	m.HandleFunc("GET /matter/charge", s.page(s.staffOnly(s.chargeForm)))
	m.HandleFunc("POST /matter/charge", s.page(s.staffOnly(s.chargeIntent)))
	m.HandleFunc("POST /matter/submit", s.page(s.staffOnly(s.paySubmit)))
	m.HandleFunc("GET /matter/refund", s.page(s.staffOnly(s.refundForm)))
	m.HandleFunc("POST /matter/refund", s.page(s.staffOnly(s.refundPost)))
	m.HandleFunc("POST /matter/issue", s.page(s.staffOnly(s.issuePost)))
	m.HandleFunc("POST /matter/deposits", s.page(s.staffOnly(s.depositsPost)))
	m.HandleFunc("GET /matter/fee-agreement", s.page(s.staffOnly(s.panel)))
	m.HandleFunc("POST /matter/fee-agreement", s.page(s.staffOnly(s.draftPost)))
	m.HandleFunc("GET /matter/send", s.page(s.staffOnly(s.panel)))
	m.HandleFunc("POST /matter/send", s.page(s.staffOnly(s.sendPost)))
	m.HandleFunc("POST /matter/resend", s.page(s.staffOnly(s.resendPost)))
	m.HandleFunc("POST /matter/void", s.page(s.staffOnly(s.voidPost)))
	m.HandleFunc("GET /tab/agreements", s.page(s.staffOnly(s.agreementsTab)))
	m.HandleFunc("GET /tab/reconciliation", s.page(s.staffOnly(s.reconciliationTab)))
	m.HandleFunc("POST /tab/resolve", s.page(s.staffOnly(s.resolvePost)))
	m.HandleFunc("POST /tab/record", s.page(s.staffOnly(s.recordPost)))
	m.HandleFunc("POST /tab/post-refund", s.page(s.staffOnly(s.postRefundPost)))
	m.HandleFunc("POST /tab/reopen", s.page(s.staffOnly(s.reopenPost)))
	m.HandleFunc("GET /tab/setup", s.page(s.staffOnly(s.setupTab)))
	m.HandleFunc("POST /tab/setup", s.page(s.staffOnly(s.setupTab)))
	m.HandleFunc("GET /widget/awaiting-signature", s.page(s.staffOnly(s.awaitingWidget)))
	m.HandleFunc("GET /widget/exceptions", s.page(s.staffOnly(s.exceptionsWidget)))
}

func (s *Server) payFields(ctx context.Context, in Intent, action, back string) (string, data, error) {
	st, err := s.settings(ctx)
	if err != nil {
		return "", data{"Back": back}, err
	}
	return "payfields", data{"Title": "Pay", "Intent": in, "Action": action, "Back": back, "HostedFields": st.HostedFieldsURL,
		"Static": "..", "Expires": in.ExpiresAt.Format(time.Kitchen + " MST")}, nil
}

// clientPay is the pay tab: an invoice payment or a trust deposit.
func (s *Server) clientPay(ctx context.Context, v viewer, r *http.Request) (string, data, error) {
	back := returnTo(r.FormValue("return_to"))
	d := data{"Back": back}
	st, err := s.settings(ctx)
	if err != nil {
		return "", d, err
	}
	if !st.LawPayReady() {
		return "", d, errNoLawPay
	}
	req := PayRequest{Purpose: r.FormValue("purpose"), InvoiceID: r.FormValue("invoice_id"), MatterID: r.FormValue("matter_id"),
		ReplenishmentRequestID: r.FormValue("replenishment_request_id")}
	if req.Purpose == "trust_deposit" && req.ReplenishmentRequestID == "" {
		if r.Method == http.MethodGet {
			enabled, minimum, found, err := s.matterPayability(ctx, req.MatterID)
			if err != nil {
				return "", d, err
			}
			if !found || !enabled {
				return "", d, errNotFound
			}
			return "payamount", data{"Title": "Make a deposit", "Action": "pay", "MatterID": req.MatterID, "Back": back,
				"Min": max(st.MinCharge, minimum), "Max": st.MaxCharge}, nil
		}
		cents, err := ParseDollars(r.FormValue("amount"))
		if err != nil {
			return "", d, fail(http.StatusUnprocessableEntity, "validation_failed", "Enter the amount, like 250.00.")
		}
		req.AmountCents = cents
	}
	in, err := s.CreateIntent(ctx, v, req)
	if err != nil {
		return "", d, err
	}
	return s.payFields(ctx, in, "submit", back)
}

// paySubmit charges a pay intent and shows the result.
func (s *Server) paySubmit(ctx context.Context, v viewer, r *http.Request) (string, data, error) {
	back := returnTo(r.FormValue("return_to"))
	if !v.Client() {
		back = "../matter"
	}
	d := data{"Back": back}
	token := r.FormValue("token")
	if token == "" {
		return "", d, fail(http.StatusUnprocessableEntity, "validation_failed", "Enter the card or bank details.")
	}
	res, err := s.Submit(ctx, v, r.FormValue("intent_id"), token)
	if err != nil {
		return "", d, err
	}
	return "payresult", data{"Title": "Payment", "Result": res, "Error": res.State != "recorded_completed" && res.State != "recorded_pending", "Back": back}, nil
}

// clientSign is the sign tab.
func (s *Server) clientSign(ctx context.Context, v viewer, r *http.Request) (string, data, error) {
	back := returnTo(r.FormValue("return_to"))
	d := data{"Back": back}
	g, found, err := s.signingBy(ctx, "id", r.FormValue("signing_request_id"))
	if err != nil {
		return "", d, err
	}
	if !found || v.ContactID == "" || g.ClientContactID != v.ContactID || g.State == "sending" || g.State == "error" {
		return "", d, errNotFound
	}
	return "sign", data{"Title": "Fee agreement", "Signing": g, "URL": template.URL(g.signingURL), "Back": back}, nil
}

func (s *Server) chargeForm(ctx context.Context, v viewer, r *http.Request) (string, data, error) {
	st, err := s.settings(ctx)
	if err != nil {
		return "", nil, err
	}
	if !st.LawPayReady() {
		return "", nil, errNoLawPay
	}
	if !v.canCharge() {
		return "", nil, errForbidden
	}
	matter := matterOf(v, r)
	invs, err := s.core.Invoices(ctx, v.auth(), matter)
	if err != nil {
		return "", nil, notFoundAs(err)
	}
	var open []any
	for _, inv := range invs {
		if inv.BalanceDueCents > 0 && inv.Status != "void" && inv.Status != "draft" {
			open = append(open, inv)
		}
	}
	return "charge", data{"Title": "Take payment", "MatterID": matter, "Invoices": open}, nil
}

func (s *Server) chargeIntent(ctx context.Context, v viewer, r *http.Request) (string, data, error) {
	cents, err := ParseDollars(r.FormValue("amount"))
	if err != nil {
		return "", nil, fail(http.StatusUnprocessableEntity, "validation_failed", "Enter the amount, like 250.00.")
	}
	in, err := s.CreateIntent(ctx, v, PayRequest{MatterID: matterOf(v, r), AmountCents: cents, Destination: r.FormValue("destination"),
		InvoiceID: r.FormValue("invoice_id")})
	if err != nil {
		return "", nil, err
	}
	return s.payFields(ctx, in, "submit", "../matter")
}

func (s *Server) refundForm(ctx context.Context, v viewer, r *http.Request) (string, data, error) {
	st, err := s.settings(ctx)
	if err != nil {
		return "", nil, err
	}
	if !st.LawPayReady() {
		return "", nil, errNoLawPay
	}
	if !v.canRefund() {
		return "refund", data{"Title": "Refund", "CanRefund": false}, nil
	}
	p, _, left, err := s.refundable(ctx, v.auth(), r.FormValue("payment_id"))
	if err != nil {
		return "", nil, err
	}
	amount := left
	if a := r.FormValue("amount"); a != "" {
		if c, err := ParseDollars(a); err == nil {
			amount = c
		}
	}
	return "refund", data{"Title": "Refund", "CanRefund": true, "Payment": p, "Left": left, "Amount": amount,
		"Warning": s.TrustWarning(ctx, v, p, amount)}, nil
}

func (s *Server) refundPost(ctx context.Context, v viewer, r *http.Request) (string, data, error) {
	cents, err := ParseDollars(r.FormValue("amount"))
	if err != nil {
		return "", nil, fail(http.StatusUnprocessableEntity, "validation_failed", "Enter the amount, like 250.00.")
	}
	res, err := s.Refund(ctx, v, RefundRequest{PaymentID: r.FormValue("payment_id"), AmountCents: cents, Reason: r.FormValue("reason")})
	if err != nil {
		return "", nil, err
	}
	return "message", data{"Title": "Refund", "Message": res.Message, "Detail": res.Warning, "Back": "../matter"}, nil
}

func (s *Server) issuePost(ctx context.Context, v viewer, r *http.Request) (string, data, error) {
	res, err := s.IssueProposed(ctx, v, r.FormValue("refund_id"))
	if err != nil {
		return "", nil, err
	}
	return "message", data{"Title": "Refund", "Message": res.Message, "Back": "../matter"}, nil
}

func (s *Server) depositsPost(ctx context.Context, v viewer, r *http.Request) (string, data, error) {
	if err := s.SetDeposits(ctx, v, matterOf(v, r), r.FormValue("enabled") == "true"); err != nil {
		return "", nil, err
	}
	return "message", data{"Title": "Client trust deposits", "Message": "Saved.", "Back": "../matter"}, nil
}

func (s *Server) matterTab(ctx context.Context, v viewer, r *http.Request) (string, data, error) {
	matter := matterOf(v, r)
	if !s.canSee(ctx, v, matter) {
		return "", nil, errNotFound
	}
	all, err := s.Transactions(ctx, v, "", "")
	if err != nil {
		return "", nil, err
	}
	var ts []Transaction
	for _, t := range all {
		if t.MatterID == matter {
			ts = append(ts, t)
		}
	}
	rows, err := s.c.DB.QueryContext(ctx, `SELECT r.id, r.amount_cents, r.reason FROM lawpay_refunds r JOIN lawpay_charges c ON c.id = r.charge_id
		WHERE c.matter_id = $1 AND r.state = 'proposed'`, matter)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	type proposed struct {
		ID, Reason  string
		AmountCents int64
	}
	var ps []proposed
	for rows.Next() {
		var p proposed
		if err := rows.Scan(&p.ID, &p.AmountCents, &p.Reason); err != nil {
			return "", nil, err
		}
		ps = append(ps, p)
	}
	on := true
	var enabled bool
	if err := s.c.DB.QueryRowContext(ctx, `SELECT deposits_enabled FROM matter_deposits WHERE matter_id = $1`, matter).Scan(&enabled); err == nil {
		on = enabled
	}
	return "matter", data{"Title": "Payments and signing", "MatterID": matter, "Transactions": ts, "Proposed": ps,
		"CanRefund": v.canRefund(), "DepositsOn": on}, nil
}

// panel is the fee agreement panel (and the Send action's page).
func (s *Server) panel(ctx context.Context, v viewer, r *http.Request) (string, data, error) {
	st, err := s.settings(ctx)
	if err != nil {
		return "", nil, err
	}
	if !st.DocuSealReady() && strings.HasSuffix(r.URL.Path, "/send") {
		return "", nil, errNoDocuSeal
	}
	matter := matterOf(v, r)
	m, err := s.core.GetMatter(ctx, v.auth(), matter)
	if err != nil {
		return "", nil, notFoundAs(err)
	}
	fas, err := s.core.FeeAgreements(ctx, v.auth(), matter)
	if err != nil {
		return "", nil, err
	}
	draft := ""
	for _, fa := range fas {
		if fa.Status == "draft" {
			draft = fa.ID
		}
	}
	gs, err := s.signings(ctx, `matter_id = $1`, matter)
	if err != nil {
		return "", nil, err
	}
	return "panel", data{"Title": "Fee agreement", "MatterID": matter, "Agreements": fas, "Signings": gs, "DraftID": draft,
		"ReadOnly": m.Status == "closed" || !v.canCharge()}, nil
}

func formAmount(r *http.Request) (*int64, error) {
	a := strings.TrimSpace(r.FormValue("amount"))
	if a == "" {
		return nil, nil
	}
	c, err := ParseDollars(a)
	if err != nil {
		return nil, fail(http.StatusUnprocessableEntity, "validation_failed", "Enter the amount, like 1500.00.")
	}
	return &c, nil
}

func (s *Server) draftPost(ctx context.Context, v viewer, r *http.Request) (string, data, error) {
	amount, err := formAmount(r)
	if err != nil {
		return "", nil, err
	}
	nc := r.FormValue("new_case") == "true"
	if _, err := s.SaveDraft(ctx, v, AgreementRequest{ID: r.FormValue("id"), MatterID: matterOf(v, r), FeeType: r.FormValue("fee_type"),
		AmountCents: amount, ServicesDescription: r.FormValue("services_description"), CaseNumber: r.FormValue("case_number"), NewCase: &nc}); err != nil {
		return "", nil, err
	}
	return "message", data{"Title": "Fee agreement", "Message": "Draft saved.", "Back": "fee-agreement"}, nil
}

func (s *Server) sendPost(ctx context.Context, v viewer, r *http.Request) (string, data, error) {
	g, err := s.Send(ctx, v, r.FormValue("agreement_id"))
	if err != nil {
		return "", data{"Back": "fee-agreement"}, err
	}
	msg := "Sent for signature. The client gets an email with a link to sign in the portal."
	if g.InviteError != "" {
		msg = "Sent for signature, but " + strings.ToLower(g.InviteError[:1]) + g.InviteError[1:]
	}
	return "message", data{"Title": "Fee agreement", "Message": msg, "Back": "fee-agreement"}, nil
}

func (s *Server) resendPost(ctx context.Context, v viewer, r *http.Request) (string, data, error) {
	if _, err := s.Resend(ctx, v, r.FormValue("signing_id")); err != nil {
		return "", data{"Back": "fee-agreement"}, err
	}
	return "message", data{"Title": "Fee agreement", "Message": "The signing link was sent again.", "Back": "fee-agreement"}, nil
}

func (s *Server) voidPost(ctx context.Context, v viewer, r *http.Request) (string, data, error) {
	if _, err := s.Void(ctx, v, r.FormValue("signing_id"), r.FormValue("reason")); err != nil {
		return "", data{"Back": "fee-agreement"}, err
	}
	return "message", data{"Title": "Fee agreement", "Message": "Voided. The client was told they don't need to sign.", "Back": "fee-agreement"}, nil
}

func (s *Server) agreementsTab(ctx context.Context, v viewer, r *http.Request) (string, data, error) {
	gs, err := s.Agreements(ctx, v, "", r.FormValue("state"))
	if err != nil {
		return "", nil, err
	}
	return "agreements", data{"Title": "Fee agreements", "Signings": gs}, nil
}

func (s *Server) reconciliationTab(ctx context.Context, v viewer, r *http.Request) (string, data, error) {
	st, err := s.settings(ctx)
	if err != nil {
		return "", nil, err
	}
	if !st.LawPayReady() {
		return "", nil, errNoLawPay
	}
	ts, err := s.Transactions(ctx, v, r.FormValue("state"), r.FormValue("kind"))
	if err != nil {
		return "", nil, err
	}
	var es []Exception
	if v.Has("admin", "attorney") {
		if es, err = s.Exceptions(ctx, v, r.FormValue("kind"), ""); err != nil {
			return "", nil, err
		}
	}
	rows, err := s.c.DB.QueryContext(ctx, `SELECT invoice_id FROM payable_invoices WHERE status = 'funded' ORDER BY opened_at`)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	var funded []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", nil, err
		}
		funded = append(funded, id)
	}
	return "reconciliation", data{"Title": "LawPay reconciliation", "Transactions": ts, "Exceptions": es, "Funded": funded}, nil
}

func backTo(r *http.Request) string { return "reconciliation?" + url.Values{"context": {contextToken(r)}}.Encode() }

func (s *Server) resolvePost(ctx context.Context, v viewer, r *http.Request) (string, data, error) {
	if err := s.Resolve(ctx, v, r.FormValue("exception_id"), r.FormValue("note")); err != nil {
		return "", data{"Back": backTo(r)}, err
	}
	return "message", data{"Title": "Resolved", "Message": "The exception is resolved.", "Back": backTo(r)}, nil
}

func (s *Server) recordPost(ctx context.Context, v viewer, r *http.Request) (string, data, error) {
	if _, err := s.RecordUnknown(ctx, v, r.FormValue("exception_id"), r.FormValue("matter_id"), r.FormValue("contact_id")); err != nil {
		return "", data{"Back": backTo(r)}, err
	}
	return "message", data{"Title": "Recorded", "Message": "The LawPay charge is recorded on the matter.", "Back": backTo(r)}, nil
}

func (s *Server) postRefundPost(ctx context.Context, v viewer, r *http.Request) (string, data, error) {
	if _, err := s.PostExternal(ctx, v, r.FormValue("exception_id"), r.FormValue("note")); err != nil {
		return "", data{"Back": backTo(r)}, err
	}
	return "message", data{"Title": "Posted", "Message": "The LawPay refund is posted.", "Back": backTo(r)}, nil
}

func (s *Server) reopenPost(ctx context.Context, v viewer, r *http.Request) (string, data, error) {
	if err := s.Reopen(ctx, v, r.FormValue("invoice_id")); err != nil {
		return "", data{"Back": backTo(r)}, err
	}
	return "message", data{"Title": "Reopened", "Message": "Online payment is open again on the invoice.", "Back": backTo(r)}, nil
}

func (s *Server) setupTab(ctx context.Context, v viewer, r *http.Request) (string, data, error) {
	if !v.Has("admin") {
		return "", nil, errForbidden
	}
	d := data{"Title": "LawPay and DocuSeal setup", "SettingsLink": settingsLink, "LawPay": "not checked", "Trust": "not checked", "DocuSeal": "not checked"}
	if r.Method == http.MethodPost {
		res, err := s.TestConnection(ctx, v)
		if err != nil {
			return "", nil, err
		}
		d["LawPay"], d["Trust"], d["DocuSeal"] = res.LawPay, res.Trust, res.DocuSeal
	} else {
		st, err := s.settings(ctx)
		if err != nil {
			return "", nil, err
		}
		ready := map[bool]string{true: "set", false: "not set up"}
		d["LawPay"], d["Trust"], d["DocuSeal"] = ready[st.LawPayReady()], ready[st.TrustConfigured()], ready[st.DocuSealReady()]
	}
	return "setup", d, nil
}

func (s *Server) awaitingWidget(ctx context.Context, v viewer, r *http.Request) (string, data, error) {
	n, err := s.AwaitingCount(ctx, v)
	if err != nil {
		return "", nil, err
	}
	return "widget_awaiting", data{"Title": "Fee agreements awaiting signature", "Sent": n["sent"], "ClientSigned": n["client_signed"],
		"Link": "/modules/" + ID + "/agreements?state="}, nil
}

func (s *Server) exceptionsWidget(ctx context.Context, v viewer, r *http.Request) (string, data, error) {
	es, err := s.Exceptions(ctx, v, "", "open")
	if err != nil {
		if errors.Is(err, errForbidden) {
			return "", nil, errNotFound
		}
		return "", nil, err
	}
	counts := map[string]int{}
	for _, e := range es {
		counts[e.Kind]++
	}
	type kind struct {
		Kind, Title string
		Count       int
	}
	var ks []kind
	for k, n := range counts {
		ks = append(ks, kind{Kind: k, Title: exceptionTitle(k), Count: n})
	}
	sort.Slice(ks, func(i, j int) bool { return ks[i].Kind < ks[j].Kind })
	return "widget_exceptions", data{"Title": "Payment and signing exceptions", "Kinds": ks, "Link": "/modules/" + ID + "/reconciliation?kind="}, nil
}
