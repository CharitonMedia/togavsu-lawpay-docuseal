package fakecore

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type request struct {
	c          *Core
	r          *http.Request
	body       []byte
	params     map[string]string
	caller     caller
	deliveries []delivery
}

func (q *request) decode(v any) error {
	if len(q.body) == 0 {
		return nil
	}
	if err := json.Unmarshal(q.body, v); err != nil {
		return perr(http.StatusBadRequest, "bad_request", err.Error())
	}
	return nil
}

func (q *request) query(k string) string { return q.r.URL.Query().Get(k) }

type handler func(q *request) (int, any, error)

type route struct {
	method, pattern, op string
	fn                  handler
}

var routes []route

func init() {
	routes = []route{
		{"GET", "/api/v1/modules/{module}/settings", "getModuleSettings", (*request).settings},
		{"POST", "/api/v1/modules/{module}/context-tokens/verify", "verifyContextToken", (*request).verify},
		{"GET", "/api/v1/modules/{module}/views/{view}", "readModuleView", (*request).view},
		{"GET", "/api/v1/matters/{id}", "getMatter", (*request).getMatter},
		{"PATCH", "/api/v1/matters/{id}", "updateMatter", (*request).patchMatter},
		{"GET", "/api/v1/matter-parties", "listMatterParty", (*request).parties},
		{"GET", "/api/v1/fee-agreements", "listFeeAgreement", (*request).listFees},
		{"POST", "/api/v1/fee-agreements", "createFeeAgreement", (*request).createFee},
		{"GET", "/api/v1/fee-agreements/{id}", "getFeeAgreement", (*request).getFee},
		{"PATCH", "/api/v1/fee-agreements/{id}", "updateFeeAgreement", (*request).patchFee},
		{"GET", "/api/v1/contacts/{id}", "getContact", (*request).getContact},
		{"GET", "/api/v1/contact-emails", "listContactEmail", (*request).contactRows},
		{"GET", "/api/v1/contact-phones", "listContactPhone", (*request).contactRows},
		{"GET", "/api/v1/contact-addresses", "listContactAddress", (*request).contactRows},
		{"GET", "/api/v1/invoices", "listInvoice", (*request).listInvoices},
		{"GET", "/api/v1/invoices/{id}", "getInvoice", (*request).getInvoice},
		{"POST", "/api/v1/payments/processor-initializations", "initiateProcessorPayment", (*request).initiate},
		{"POST", "/api/v1/payments", "createPayment", (*request).createPayment},
		{"GET", "/api/v1/payments", "listPayment", (*request).listPayments},
		{"GET", "/api/v1/payments/{id}", "getPayment", (*request).getPayment},
		{"POST", "/api/v1/payments/{id}/status", "updatePaymentStatus", (*request).paymentStatus},
		{"POST", "/api/v1/payments/{id}/refunds", "createPaymentRefund", (*request).createRefund},
		{"GET", "/api/v1/payments/{id}/refunds", "listPaymentRefunds", (*request).listRefunds},
		{"POST", "/api/v1/payments/{id}/refunds/{refund}/status", "updatePaymentRefundStatus", (*request).refundStatus},
		{"GET", "/api/v1/trust-accounts/{id}", "getTrustAccount", (*request).trustAccount},
		{"GET", "/api/v1/trust/balances", "getTrustBalances", (*request).balances},
		{"GET", "/api/v1/trust-replenishment-requests/{id}", "getTrustReplenishment", (*request).replenishment},
		{"GET", "/api/v1/client-access", "getClientAccess", (*request).clientAccess},
		{"POST", "/api/v1/file-uploads", "createFileUpload", (*request).createUpload},
		{"PUT", "/api/v1/file-uploads/{id}", "putFileChunk", (*request).putChunk},
		{"POST", "/api/v1/file-uploads/{id}/complete", "completeFileUpload", (*request).completeUpload},
		{"POST", "/api/v1/files", "publishFile", (*request).publish},
		{"POST", "/api/v1/files/publication-grants", "issueFilePublicationGrant", (*request).fileGrant},
		{"GET", "/api/v1/files/{id}", "getFile", (*request).getFile},
		{"POST", "/api/v1/files/{id}/status", "markFileStatus", (*request).fileStatus},
		{"POST", "/api/v1/events", "createEvent", (*request).createEvent},
		{"POST", "/api/v1/notifications", "createNotification", (*request).notify},
		{"POST", "/api/v1/mail/messages", "sendModuleMail", (*request).mail},
		{"GET", "/api/v1/conflict-checks/{id}", "getConflictCheck", (*request).conflict},
		{"GET", "/api/v1/books/profile", "getBooksProfile", (*request).booksProfile},
		{"GET", "/api/v1/books/matters/{id}", "getBooksMatter", (*request).booksMatter},
	}
}

func match(method, path string) (route, map[string]string) {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for _, rt := range routes {
		if rt.method != method {
			continue
		}
		ps := strings.Split(strings.Trim(rt.pattern, "/"), "/")
		if len(ps) != len(segs) {
			continue
		}
		params := map[string]string{}
		ok := true
		for i, p := range ps {
			if strings.HasPrefix(p, "{") {
				params[strings.Trim(p, "{}")] = segs[i]
			} else if p != segs[i] {
				ok = false
				break
			}
		}
		if ok {
			return rt, params
		}
	}
	return route{op: "unknown " + method + " " + path}, nil
}

// ---- access ----

func (q *request) staff() bool {
	return q.caller.kind == "viewer" && q.caller.user.has("admin", "attorney", "staff", "accountant")
}

// canMatter applies the viewer's (or the books reader's) access to a matter.
func (q *request) canMatter(id string) bool {
	m, ok := q.c.matters[id]
	if !ok {
		return false
	}
	w := q.c.walls[id]
	switch q.caller.kind {
	case "viewer":
		u := q.caller.user
		if u.client() {
			return m.ClientContact == u.ContactID && u.ContactID != ""
		}
		return w == nil || w[u.ID]
	default:
		return w == nil
	}
}

func (q *request) viewerOrService() error {
	if q.caller.kind == "task" {
		return errScope
	}
	return nil
}

// ---- module ----

func (q *request) settings() (int, any, error) {
	var out []map[string]any
	for k, v := range q.c.settings {
		val := v
		if q.caller.kind == "viewer" && q.c.secretKey[k] {
			val = "Redacted"
		}
		out = append(out, map[string]any{"key": k, "value": val, "secret": q.c.secretKey[k]})
	}
	return http.StatusOK, out, nil
}

func (q *request) verify() (int, any, error) {
	if q.caller.kind != "viewer" {
		return 0, nil, perr(http.StatusUnauthorized, "unauthenticated", "no context token")
	}
	u := q.caller.user
	out := map[string]any{"viewer_user_id": u.ID, "roles": u.Roles, "module": ModuleID, "matter_id": nil}
	if q.caller.matter != "" {
		out["matter_id"] = q.caller.matter
	}
	if u.client() {
		out["contact_id"] = u.ContactID
	}
	return http.StatusOK, out, nil
}

// view reads a published view inside the viewer's access, as the core does.
func (q *request) view() (int, any, error) {
	view := q.params["view"]
	if !q.c.scopes["views:"+ModuleID+"."+view+":read"] {
		return 0, nil, errForbidden
	}
	keys := map[string]string{"invoice_payability": "invoice_id", "matter_payability": "matter_id", "signature_requests": "file_id"}
	key, ok := keys[view]
	if !ok {
		return 0, nil, errNotFound
	}
	id := q.query("record_id")
	allowed := false
	switch view {
	case "invoice_payability":
		inv, ok := q.c.invoices[id]
		allowed = ok && q.canMatter(inv.MatterID) && (!q.caller.user.client() || inv.Status != "draft")
	case "matter_payability":
		allowed = q.canMatter(id)
	case "signature_requests":
		f, ok := q.c.files[id]
		allowed = ok && q.canMatter(f.MatterID) && (!q.caller.user.client() || f.Shared)
	}
	if !allowed {
		return http.StatusOK, map[string]any{"data": []any{}}, nil
	}
	rows, err := q.c.viewDB.Query(`SELECT row_to_json(v) FROM `+view+` v WHERE `+key+` = $1`, id)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	out := []json.RawMessage{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return 0, nil, err
		}
		out = append(out, raw)
	}
	return http.StatusOK, map[string]any{"data": out}, rows.Err()
}

// ---- matters and contacts ----

func (q *request) matterOut(m *Matter) map[string]any {
	return map[string]any{"id": m.ID, "number": m.Number, "name": m.Name, "status": m.Status, "case_number": m.CaseNumber,
		"billing_type": m.BillingType, "flat_fee_cents": m.FlatFeeCents, "responsible_attorney_user_id": m.ResponsibleAttorney}
}

func (q *request) getMatter() (int, any, error) {
	if q.caller.kind != "viewer" || !q.canMatter(q.params["id"]) {
		return 0, nil, errNotFound
	}
	return http.StatusOK, q.matterOut(q.c.matters[q.params["id"]]), nil
}

func (q *request) patchMatter() (int, any, error) {
	if q.caller.kind != "viewer" || !q.canMatter(q.params["id"]) || q.caller.user.client() {
		return 0, nil, errNotFound
	}
	var body map[string]json.RawMessage
	if err := q.decode(&body); err != nil {
		return 0, nil, err
	}
	m := q.c.matters[q.params["id"]]
	if raw, ok := body["billing_type"]; ok {
		var s string
		_ = json.Unmarshal(raw, &s)
		m.BillingType = &s
	}
	if raw, ok := body["flat_fee_cents"]; ok {
		var n int64
		_ = json.Unmarshal(raw, &n)
		m.FlatFeeCents = &n
	}
	return http.StatusOK, q.matterOut(m), nil
}

func (q *request) parties() (int, any, error) {
	id := q.query("matter_id")
	if q.caller.kind != "viewer" || !q.canMatter(id) {
		return http.StatusOK, map[string]any{"data": []any{}}, nil
	}
	m := q.c.matters[id]
	return http.StatusOK, map[string]any{"data": []any{map[string]any{"id": "par_" + m.ID, "matter_id": m.ID, "contact_id": m.ClientContact,
		"role": "client", "primary": true}}}, nil
}

func (q *request) contactVisible(id string) bool {
	if q.caller.kind != "viewer" || !q.staff() {
		return false
	}
	for mid, m := range q.c.matters {
		if m.ClientContact == id && q.canMatter(mid) {
			return true
		}
	}
	return false
}

func (q *request) getContact() (int, any, error) {
	ct, ok := q.c.contacts[q.params["id"]]
	if !ok || !q.contactVisible(ct.ID) {
		return 0, nil, errNotFound
	}
	return http.StatusOK, map[string]any{"id": ct.ID, "kind": "person", "given_name": ct.Given, "family_name": ct.Family,
		"full_name": strings.TrimSpace(ct.Given + " " + ct.Family)}, nil
}

func (q *request) contactRows() (int, any, error) {
	ct, ok := q.c.contacts[q.query("contact_id")]
	if !ok || !q.contactVisible(ct.ID) {
		return http.StatusOK, map[string]any{"data": []any{}}, nil
	}
	var row map[string]any
	switch {
	case strings.HasSuffix(q.r.URL.Path, "emails"):
		if ct.Email == "" {
			return http.StatusOK, map[string]any{"data": []any{}}, nil
		}
		row = map[string]any{"email": ct.Email, "primary": true, "effective_to": nil}
	case strings.HasSuffix(q.r.URL.Path, "phones"):
		if ct.Phone == "" {
			return http.StatusOK, map[string]any{"data": []any{}}, nil
		}
		row = map[string]any{"e164": ct.Phone, "primary": true, "effective_to": nil}
	default:
		row = map[string]any{"line1": ct.Line1, "line2": ct.Line2, "city": ct.City, "region": ct.Region, "postal_code": ct.Postal,
			"primary": true, "effective_to": nil}
	}
	return http.StatusOK, map[string]any{"data": []any{row}}, nil
}

func (q *request) clientAccess() (int, any, error) {
	if q.caller.kind != "viewer" || !q.caller.user.has("admin", "attorney", "staff") {
		return 0, nil, errScope
	}
	a, ok := q.c.access[q.query("contact_id")]
	if !ok {
		return 0, nil, errNotFound
	}
	return http.StatusOK, map[string]any{"user_id": a.UserID, "active": a.Active}, nil
}

// ---- fee agreements (CORE-25) ----

func feeOut(f *FeeAgreement) map[string]any {
	out := map[string]any{"id": f.ID, "matter_id": f.MatterID, "fee_type": f.FeeType, "amount_cents": f.AmountCents,
		"services_description": f.Description, "status": f.Status, "void_reason": f.VoidReason, "signed_on": nil, "signed_file_id": nil}
	if f.SignedOn != "" {
		out["signed_on"] = f.SignedOn
	}
	if f.SignedFileID != "" {
		out["signed_file_id"] = f.SignedFileID
	}
	return out
}

func (q *request) listFees() (int, any, error) {
	var out []any
	if q.caller.kind == "viewer" && q.staff() && q.canMatter(q.query("matter_id")) {
		for _, f := range q.c.fees {
			if f.MatterID == q.query("matter_id") {
				out = append(out, feeOut(f))
			}
		}
	}
	return http.StatusOK, map[string]any{"data": out}, nil
}

func (q *request) createFee() (int, any, error) {
	var b struct {
		MatterID    string `json:"matter_id"`
		FeeType     string `json:"fee_type"`
		AmountCents *int64 `json:"amount_cents"`
		Description string `json:"services_description"`
	}
	if err := q.decode(&b); err != nil {
		return 0, nil, err
	}
	if !q.staff() || !q.canMatter(b.MatterID) {
		return 0, nil, errNotFound
	}
	f := &FeeAgreement{ID: q.c.newID("fee"), MatterID: b.MatterID, FeeType: b.FeeType, AmountCents: b.AmountCents, Description: b.Description, Status: "draft"}
	q.c.fees[f.ID] = f
	q.c.mirror("fee_agreements", f.ID)
	return http.StatusCreated, feeOut(f), nil
}

func (q *request) getFee() (int, any, error) {
	f, ok := q.c.fees[q.params["id"]]
	if !ok || !q.staff() || !q.canMatter(f.MatterID) {
		return 0, nil, errNotFound
	}
	return http.StatusOK, feeOut(f), nil
}

func (q *request) patchFee() (int, any, error) {
	f, ok := q.c.fees[q.params["id"]]
	if !ok {
		return 0, nil, errNotFound
	}
	var b map[string]json.RawMessage
	if err := q.decode(&b); err != nil {
		return 0, nil, err
	}
	str := func(k string) string {
		var s string
		_ = json.Unmarshal(b[k], &s)
		return s
	}
	if q.caller.kind == "service" {
		// CORE-25: the signature module may move sent to signed with signed_on and
		// signed_file_id, and nothing else.
		for k := range b {
			if k != "status" && k != "signed_on" && k != "signed_file_id" {
				return 0, nil, perr(http.StatusForbidden, "field_locked", k)
			}
		}
		if f.SignatureModule != ModuleID || f.Status != "sent" || str("status") != "signed" || str("signed_on") == "" || str("signed_file_id") == "" {
			return 0, nil, errScope
		}
		f.Status, f.SignedOn, f.SignedFileID = "signed", str("signed_on"), str("signed_file_id")
		return http.StatusOK, feeOut(f), nil
	}
	if !q.staff() || !q.canMatter(f.MatterID) {
		return 0, nil, errNotFound
	}
	if raw, ok := b["fee_type"]; ok {
		_ = json.Unmarshal(raw, &f.FeeType)
	}
	if raw, ok := b["amount_cents"]; ok {
		_ = json.Unmarshal(raw, &f.AmountCents)
	}
	if _, ok := b["services_description"]; ok {
		f.Description = str("services_description")
	}
	if st := str("status"); st != "" {
		f.Status = st
		if st == "sent" {
			f.SignatureModule = ModuleID
		}
		if st == "void" {
			f.VoidReason = str("void_reason")
		}
	}
	return http.StatusOK, feeOut(f), nil
}

// ---- invoices ----

func invOut(inv *Invoice) map[string]any {
	return map[string]any{"id": inv.ID, "matter_id": inv.MatterID, "number": inv.Number, "status": inv.Status, "total_cents": inv.TotalCents,
		"amount_paid_cents": inv.PaidCents, "balance_due_cents": inv.TotalCents - inv.PaidCents}
}

func (q *request) invoiceVisible(inv *Invoice) bool {
	if !q.canMatter(inv.MatterID) {
		return false
	}
	return !(q.caller.kind == "viewer" && q.caller.user.client() && inv.Status == "draft")
}

func (q *request) getInvoice() (int, any, error) {
	inv, ok := q.c.invoices[q.params["id"]]
	if !ok || q.caller.kind == "task" || !q.invoiceVisible(inv) {
		return 0, nil, errNotFound
	}
	return http.StatusOK, invOut(inv), nil
}

func (q *request) listInvoices() (int, any, error) {
	var out []any
	for _, inv := range q.c.invoices {
		if (q.query("matter_id") == "" || inv.MatterID == q.query("matter_id")) && q.caller.kind != "task" && q.invoiceVisible(inv) {
			out = append(out, invOut(inv))
		}
	}
	return http.StatusOK, map[string]any{"data": out}, nil
}

// ---- payments (CORE-23) ----

func (q *request) initiate() (int, any, error) {
	var b struct {
		Processor      string `json:"processor_module_id"`
		ContactID      string `json:"contact_id"`
		MatterID       string `json:"matter_id"`
		AmountCents    int64  `json:"amount_cents"`
		Destination    string `json:"destination"`
		TrustAccountID string `json:"trust_account_id"`
		InvoiceID      string `json:"invoice_id"`
		Replenishment  string `json:"replenishment_request_id"`
	}
	if err := q.decode(&b); err != nil {
		return 0, nil, err
	}
	if q.caller.kind != "viewer" {
		return 0, nil, errScope
	}
	u := q.caller.user
	if !q.canMatter(b.MatterID) {
		return 0, nil, errNotFound
	}
	if u.client() {
		if b.ContactID != u.ContactID {
			return 0, nil, errForbidden
		}
		if b.Destination == "operating" {
			inv, ok := q.c.invoices[b.InvoiceID]
			if !ok || inv.MatterID != b.MatterID || inv.Status == "draft" || inv.TotalCents-inv.PaidCents <= 0 {
				return 0, nil, errNotFound
			}
		}
	} else if !u.has("admin", "attorney", "staff") {
		return 0, nil, errForbidden
	}
	if b.Processor != ModuleID || b.AmountCents <= 0 {
		return 0, nil, perr(http.StatusUnprocessableEntity, "validation_failed", "bad initialization")
	}
	if b.Destination == "trust" && !q.c.accounts[b.TrustAccountID] {
		return 0, nil, perr(http.StatusUnprocessableEntity, "validation_failed", "trust account not active")
	}
	if key := q.r.Header.Get("Idempotency-Key"); key != "" {
		if id, ok := q.c.idem["init:"+key]; ok {
			in := q.c.inits[id]
			return http.StatusOK, map[string]any{"id": in.ID, "payment_id": in.PaymentID, "status": "authorized"}, nil
		}
	}
	in := &initialization{ID: q.c.newID("ini"), PaymentID: q.c.newID("pay")}
	in.grant = grant{Kind: "payment", MatterID: b.MatterID, ContactID: b.ContactID, Destination: b.Destination, InvoiceID: b.InvoiceID,
		InitID: in.ID, PaymentID: in.PaymentID, AmountCents: b.AmountCents}
	q.c.inits[in.ID] = in
	if key := q.r.Header.Get("Idempotency-Key"); key != "" {
		q.c.idem["init:"+key] = in.ID
	}
	cred := "grant-" + q.c.newID("prc")
	q.c.grants[cred] = in.grant
	exp := q.c.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if !q.c.holdGrant {
		q.deliveries = append(q.deliveries, q.c.event("payment.processor_authorized", "payment_processor_initialization", in.ID, b.MatterID,
			map[string]any{"processor_authorization": map[string]any{"initialization_id": in.ID, "payment_id": in.PaymentID, "expires_at": exp}}, cred))
	}
	return http.StatusCreated, map[string]any{"id": in.ID, "payment_id": in.PaymentID, "status": "authorized", "expires_at": exp}, nil
}

func payOut(p *Payment) map[string]any {
	state := "none"
	switch {
	case p.RefundedCents > 0 && p.RefundedCents >= p.AmountCents:
		state = "full"
	case p.RefundedCents > 0:
		state = "partial"
	}
	nul := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	return map[string]any{"id": p.ID, "created_at": p.Created.UTC().Format(time.RFC3339Nano), "contact_id": p.ContactID, "matter_id": p.MatterID,
		"amount_cents": p.AmountCents, "destination": p.Destination, "method": p.Method, "reference_number": p.Reference, "status": p.Status,
		"invoice_id": nul(p.InvoiceID), "trust_account_id": nul(p.TrustAccountID), "source": p.Source, "refunded_cents": p.RefundedCents,
		"refund_state": state}
}

// complete posts a payment's completion: the trust deposit, the invoice amount paid and
// the replenishment request.
func (q *request) complete(p *Payment) {
	if p.Destination == "trust" {
		q.c.trust = append(q.c.trust, TrustEntry{ID: q.c.newID("tle"), MatterID: p.MatterID, ClientID: p.ContactID, AccountID: p.TrustAccountID,
			Type: "deposit", PaymentID: p.ID, Cents: p.AmountCents})
	}
	if inv, ok := q.c.invoices[p.InvoiceID]; ok {
		inv.PaidCents += p.AmountCents
		if inv.TotalCents-inv.PaidCents <= 0 {
			inv.Status = "paid"
		}
	}
	if r, ok := q.c.repl[p.ReplenishmentID]; ok && p.Destination == "trust" {
		r.Status = "satisfied"
	}
	q.deliveries = append(q.deliveries, q.c.event("payment.completed", "payment", p.ID, p.MatterID, nil, ""))
}

func (q *request) createPayment() (int, any, error) {
	var b struct {
		ContactID      string `json:"contact_id"`
		MatterID       string `json:"matter_id"`
		AmountCents    int64  `json:"amount_cents"`
		Destination    string `json:"destination"`
		TrustAccountID string `json:"trust_account_id"`
		Method         string `json:"method"`
		Reference      string `json:"reference_number"`
		Status         string `json:"status"`
		InvoiceID      string `json:"invoice_id"`
		Source         string `json:"source"`
		Replenishment  string `json:"replenishment_request_id"`
	}
	if err := q.decode(&b); err != nil {
		return 0, nil, err
	}
	if b.Status == "" {
		b.Status = "pending"
	}
	id := ""
	switch q.caller.kind {
	case "task":
		g, ok := q.c.grants[q.caller.task]
		if !ok || g.Kind != "payment" {
			return 0, nil, errForbidden
		}
		if g.AmountCents != b.AmountCents || g.Destination != b.Destination || g.MatterID != b.MatterID || g.ContactID != b.ContactID {
			return 0, nil, errForbidden
		}
		id = g.PaymentID
		if p, ok := q.c.payments[id]; ok {
			if p.body == string(q.body) {
				return http.StatusOK, payOut(p), nil
			}
			return 0, nil, perr(http.StatusConflict, "conflict", "initialization already bound")
		}
	case "viewer":
		if !q.caller.user.has("admin", "attorney", "staff") || !q.canMatter(b.MatterID) {
			return 0, nil, errForbidden
		}
		if key := q.r.Header.Get("Idempotency-Key"); key != "" {
			if pid, ok := q.c.idem["pay:"+key]; ok {
				return http.StatusOK, payOut(q.c.payments[pid]), nil
			}
		}
		id = q.c.newID("pay")
		if key := q.r.Header.Get("Idempotency-Key"); key != "" {
			q.c.idem["pay:"+key] = id
		}
	default:
		return 0, nil, errScope
	}
	if b.Destination == "trust" && !q.c.accounts[b.TrustAccountID] {
		return 0, nil, perr(http.StatusUnprocessableEntity, "validation_failed", "trust account not active")
	}
	p := &Payment{ID: id, ContactID: b.ContactID, MatterID: b.MatterID, AmountCents: b.AmountCents, Destination: b.Destination, Method: b.Method,
		Reference: b.Reference, Status: b.Status, InvoiceID: b.InvoiceID, TrustAccountID: b.TrustAccountID, Source: b.Source,
		ReplenishmentID: b.Replenishment, Created: q.c.Now(), body: string(q.body)}
	q.c.payments[id] = p
	q.c.mirror("payments", id)
	if p.Status == "completed" {
		q.complete(p)
	}
	return http.StatusCreated, payOut(p), nil
}

func (q *request) paymentVisible(p *Payment) bool {
	switch q.caller.kind {
	case "viewer":
		return q.staff() && q.canMatter(p.MatterID)
	case "service":
		return q.canMatter(p.MatterID)
	}
	return false
}

func (q *request) getPayment() (int, any, error) {
	p, ok := q.c.payments[q.params["id"]]
	if !ok || !q.paymentVisible(p) {
		return 0, nil, errNotFound
	}
	return http.StatusOK, payOut(p), nil
}

func (q *request) listPayments() (int, any, error) {
	out := []any{}
	for _, p := range q.c.payments {
		if q.query("matter_id") != "" && p.MatterID != q.query("matter_id") {
			continue
		}
		if q.query("status") != "" && p.Status != q.query("status") {
			continue
		}
		if q.paymentVisible(p) {
			out = append(out, payOut(p))
		}
	}
	return http.StatusOK, map[string]any{"data": out}, nil
}

func (q *request) paymentStatus() (int, any, error) {
	p, ok := q.c.payments[q.params["id"]]
	if !ok {
		return 0, nil, errNotFound
	}
	var b struct {
		Status string `json:"status"`
	}
	if err := q.decode(&b); err != nil {
		return 0, nil, err
	}
	switch q.caller.kind {
	case "task":
		g, ok := q.c.grants[q.caller.task]
		if !ok || g.PaymentID != p.ID {
			return 0, nil, errForbidden
		}
		// C4: the grant stays valid until the payment leaves pending.
		if p.Status != "pending" && p.Status != b.Status {
			return 0, nil, errForbidden
		}
	case "viewer":
		if !q.caller.user.has("admin", "attorney", "staff") {
			return 0, nil, errForbidden
		}
	default:
		return 0, nil, errScope
	}
	if p.Status == b.Status {
		return http.StatusOK, payOut(p), nil
	}
	if p.Status != "pending" {
		return 0, nil, perr(http.StatusConflict, "conflict", "a settled payment doesn't regress")
	}
	p.Status = b.Status
	if b.Status == "completed" {
		q.complete(p)
	}
	return http.StatusOK, payOut(p), nil
}

// ---- refunds (CORE-24) ----

func refundOut(r *Refund) map[string]any {
	return map[string]any{"id": r.ID, "payment_id": r.PaymentID, "amount_cents": r.AmountCents, "reason": r.Reason,
		"reference_number": r.Reference, "status": r.Status, "source": r.Source}
}

func (q *request) createRefund() (int, any, error) {
	p, ok := q.c.payments[q.params["id"]]
	if !ok || q.caller.kind != "viewer" || !q.staff() || !q.canMatter(p.MatterID) {
		return 0, nil, errNotFound
	}
	if !q.caller.user.has("admin", "attorney") {
		return 0, nil, errForbidden
	}
	var b struct {
		AmountCents int64  `json:"amount_cents"`
		Reason      string `json:"reason"`
		Reference   string `json:"reference_number"`
		Status      string `json:"status"`
	}
	if err := q.decode(&b); err != nil {
		return 0, nil, err
	}
	if key := q.r.Header.Get("Idempotency-Key"); key != "" {
		if id, ok := q.c.idem["ref:"+key]; ok {
			return http.StatusOK, refundOut(q.c.refunds[id]), nil
		}
	}
	if p.Status != "completed" {
		return 0, nil, perr(http.StatusConflict, "conflict", "the payment isn't completed")
	}
	var used int64
	for _, r := range q.c.refunds {
		if r.PaymentID == p.ID && (r.Status == "pending" || r.Status == "completed") {
			used += r.AmountCents
		}
	}
	if b.AmountCents <= 0 || b.AmountCents > p.AmountCents-used {
		return 0, nil, &problem{status: http.StatusUnprocessableEntity, code: "validation_failed", errorCode: "refund_exceeds_payment",
			message: "refund_exceeds_payment", field: "amount_cents"}
	}
	r := &Refund{ID: q.c.newID("ref"), PaymentID: p.ID, AmountCents: b.AmountCents, Reason: b.Reason, Reference: b.Reference, Status: "pending", Source: p.Source}
	q.c.refunds[r.ID] = r
	q.c.mirror("payment_refunds", r.ID)
	if key := q.r.Header.Get("Idempotency-Key"); key != "" {
		q.c.idem["ref:"+key] = r.ID
	}
	if b.Status == "completed" {
		q.completeRefund(p, r)
	} else if p.Source == ModuleID && !q.c.holdGrant {
		cred := "grant-" + q.c.newID("rfg")
		q.c.grants[cred] = grant{Kind: "refund", PaymentID: p.ID, RefundID: r.ID}
		q.deliveries = append(q.deliveries, q.c.event("payment.refund_authorized", "payment_refund", r.ID, p.MatterID,
			map[string]any{"refund_authorization": map[string]any{"refund_id": r.ID, "payment_id": p.ID}}, cred))
	}
	return http.StatusCreated, refundOut(r), nil
}

func (q *request) completeRefund(p *Payment, r *Refund) {
	r.Status = "completed"
	p.RefundedCents += r.AmountCents
	if p.Destination == "trust" {
		q.c.trust = append(q.c.trust, TrustEntry{ID: q.c.newID("tle"), MatterID: p.MatterID, ClientID: p.ContactID, AccountID: p.TrustAccountID,
			Type: "refund", PaymentID: p.ID, Cents: -r.AmountCents})
		bal := q.c.balance(p.MatterID, "", "")
		if bal < 0 {
			q.c.TrustFlags = append(q.c.TrustFlags, TrustFlag{MatterID: p.MatterID, BalanceCents: bal})
			q.c.Notifications = append(q.c.Notifications, Notification{Kind: "trust.flag_raised", Title: "Trust balance below zero",
				Audience: "admins_and_responsible_attorney", MatterID: p.MatterID})
			q.c.Mails = append(q.c.Mails, Mail{MatterID: p.MatterID, Subject: "Trust balance below zero", To: []string{"admins", "responsible_attorney"}})
		}
	}
	if inv, ok := q.c.invoices[p.InvoiceID]; ok {
		inv.PaidCents -= r.AmountCents
		if inv.Status == "paid" && inv.TotalCents-inv.PaidCents > 0 {
			inv.Status = "sent"
		}
	}
	q.deliveries = append(q.deliveries, q.c.event("payment.refunded", "payment", p.ID, p.MatterID, nil, ""))
}

func (c *Core) balance(matter, client, account string) int64 {
	var n int64
	for _, e := range c.trust {
		if (matter == "" || e.MatterID == matter) && (client == "" || e.ClientID == client) && (account == "" || e.AccountID == account) {
			n += e.Cents
		}
	}
	return n
}

func (q *request) listRefunds() (int, any, error) {
	p, ok := q.c.payments[q.params["id"]]
	if !ok || !q.paymentVisible(p) {
		return 0, nil, errNotFound
	}
	out := []any{}
	for _, r := range q.c.refunds {
		if r.PaymentID == p.ID {
			out = append(out, refundOut(r))
		}
	}
	return http.StatusOK, map[string]any{"data": out}, nil
}

func (q *request) refundStatus() (int, any, error) {
	p, ok := q.c.payments[q.params["id"]]
	r, rok := q.c.refunds[q.params["refund"]]
	if !ok || !rok || r.PaymentID != p.ID {
		return 0, nil, errNotFound
	}
	switch q.caller.kind {
	case "task":
		g, ok := q.c.grants[q.caller.task]
		if !ok || g.Kind != "refund" || g.RefundID != r.ID {
			return 0, nil, errForbidden
		}
	case "viewer":
		if !q.caller.user.has("admin", "attorney") {
			return 0, nil, errForbidden
		}
	default:
		return 0, nil, errScope
	}
	var b struct {
		Status    string `json:"status"`
		Reference string `json:"reference_number"`
	}
	if err := q.decode(&b); err != nil {
		return 0, nil, err
	}
	if r.Status == b.Status {
		return http.StatusOK, refundOut(r), nil
	}
	if r.Status != "pending" {
		return 0, nil, perr(http.StatusConflict, "conflict", "refund is settled")
	}
	if b.Reference != "" {
		r.Reference = b.Reference
	}
	if b.Status == "completed" {
		q.completeRefund(p, r)
	} else {
		r.Status = b.Status
	}
	return http.StatusOK, refundOut(r), nil
}

// ---- trust ----

func (q *request) trustAccount() (int, any, error) {
	active, ok := q.c.accounts[q.params["id"]]
	if !ok || q.caller.kind == "task" {
		return 0, nil, errNotFound
	}
	return http.StatusOK, map[string]any{"id": q.params["id"], "name": "Synthetic IOLTA", "active": active}, nil
}

func (q *request) balances() (int, any, error) {
	m := q.query("matter_id")
	if q.caller.kind == "task" || (m != "" && !q.canMatter(m)) {
		return http.StatusOK, map[string]any{"data": []any{}}, nil
	}
	return http.StatusOK, map[string]any{"data": []any{map[string]any{"matter_cents": q.c.balance(m, q.query("client_id"), q.query("trust_account_id")),
		"client_cents": q.c.balance("", q.query("client_id"), q.query("trust_account_id")), "account_cents": q.c.balance("", "", q.query("trust_account_id"))}}}, nil
}

func (q *request) replenishment() (int, any, error) {
	r, ok := q.c.repl[q.params["id"]]
	if !ok || q.caller.kind != "viewer" || !q.canMatter(r.MatterID) {
		return 0, nil, errNotFound
	}
	if q.caller.user.client() {
		if r.ClientID != q.caller.user.ContactID {
			return 0, nil, errNotFound
		}
		// CORE-12 allowlist.
		return http.StatusOK, map[string]any{"id": r.ID, "matter_id": r.MatterID, "amount_cents": r.AmountCents, "status": r.Status}, nil
	}
	return http.StatusOK, map[string]any{"id": r.ID, "matter_id": r.MatterID, "client_id": r.ClientID, "amount_requested_cents": r.AmountCents, "status": r.Status}, nil
}

// ---- files ----

func (q *request) fileAuth(matter string) error {
	switch q.caller.kind {
	case "viewer":
		if !q.staff() || !q.canMatter(matter) {
			return errNotFound
		}
	case "task":
		g, ok := q.c.grants[q.caller.task]
		if !ok || g.Kind != "file" || g.MatterID != matter {
			return errForbidden
		}
	default:
		return errScope
	}
	return nil
}

func (q *request) createUpload() (int, any, error) {
	var b struct {
		MatterID string `json:"matter_id"`
		Size     int    `json:"size_bytes"`
	}
	if err := q.decode(&b); err != nil {
		return 0, nil, err
	}
	if err := q.fileAuth(b.MatterID); err != nil {
		return 0, nil, err
	}
	id := q.c.newID("upl")
	q.c.uploads[id] = nil
	return http.StatusCreated, map[string]any{"id": id}, nil
}

func (q *request) putChunk() (int, any, error) {
	if _, ok := q.c.uploads[q.params["id"]]; !ok {
		return 0, nil, errNotFound
	}
	if !strings.HasPrefix(q.r.Header.Get("Content-Range"), "bytes 0-") {
		return 0, nil, perr(http.StatusBadRequest, "bad_request", "content range")
	}
	q.c.uploads[q.params["id"]] = append([]byte(nil), q.body...)
	return http.StatusNoContent, nil, nil
}

func (q *request) completeUpload() (int, any, error) {
	if _, ok := q.c.uploads[q.params["id"]]; !ok {
		return 0, nil, errNotFound
	}
	return http.StatusOK, map[string]any{"id": q.params["id"], "status": "staged"}, nil
}

func (q *request) publish() (int, any, error) {
	var b struct {
		UploadID string `json:"upload_id"`
		MatterID string `json:"matter_id"`
		Name     string `json:"name"`
		Shared   bool   `json:"shared_with_client"`
	}
	if err := q.decode(&b); err != nil {
		return 0, nil, err
	}
	if err := q.fileAuth(b.MatterID); err != nil {
		return 0, nil, err
	}
	data, ok := q.c.uploads[b.UploadID]
	if !ok {
		return 0, nil, errNotFound
	}
	if key := q.r.Header.Get("Idempotency-Key"); key != "" {
		if id, ok := q.c.idem["file:"+key]; ok {
			return http.StatusOK, fileOut(q.c.files[id]), nil
		}
	}
	f := &File{ID: q.c.newID("fil"), MatterID: b.MatterID, Name: b.Name, Shared: b.Shared, Data: data}
	q.c.files[f.ID] = f
	q.c.mirror("files", f.ID)
	if key := q.r.Header.Get("Idempotency-Key"); key != "" {
		q.c.idem["file:"+key] = f.ID
	}
	return http.StatusCreated, fileOut(f), nil
}

func fileOut(f *File) map[string]any {
	var st any
	if f.Status != "" {
		st = f.Status
	}
	return map[string]any{"id": f.ID, "matter_id": f.MatterID, "name": f.Name, "status": st, "shared_with_client": f.Shared}
}

func (q *request) fileGrant() (int, any, error) {
	if q.caller.kind != "service" {
		return 0, nil, errScope
	}
	var b struct {
		Assignment string `json:"assignment"`
		MatterID   string `json:"matter_id"`
		Source     string `json:"source"`
	}
	if err := q.decode(&b); err != nil {
		return 0, nil, err
	}
	if b.Assignment != "matter" || q.c.matters[b.MatterID] == nil {
		return 0, nil, perr(http.StatusUnprocessableEntity, "validation_failed", "bad grant request")
	}
	cred := "grant-" + q.c.newID("fgr")
	q.c.grants[cred] = grant{Kind: "file", MatterID: b.MatterID}
	if !q.c.holdGrant {
		q.deliveries = append(q.deliveries, q.c.event("file.background_authorized", "matter", b.MatterID, b.MatterID,
			map[string]any{"background_file_authorization": map[string]any{"matter_id": b.MatterID}}, cred))
	}
	return http.StatusAccepted, map[string]any{"status": "issued"}, nil
}

func (q *request) getFile() (int, any, error) {
	f, ok := q.c.files[q.params["id"]]
	if !ok || !q.canMatter(f.MatterID) || (q.caller.kind == "viewer" && q.caller.user.client() && !f.Shared) {
		return 0, nil, errNotFound
	}
	return http.StatusOK, fileOut(f), nil
}

func (q *request) fileStatus() (int, any, error) {
	f, ok := q.c.files[q.params["id"]]
	if !ok {
		return 0, nil, errNotFound
	}
	if q.caller.kind == "service" {
		// CORE-25: only the signed file of an agreement this module signed.
		allowed := false
		for _, fa := range q.c.fees {
			if fa.SignatureModule == ModuleID && fa.MatterID == f.MatterID {
				allowed = true
			}
		}
		if !allowed {
			return 0, nil, errScope
		}
	} else if err := q.fileAuth(f.MatterID); err != nil {
		return 0, nil, err
	}
	var b struct {
		Status string `json:"status"`
	}
	if err := q.decode(&b); err != nil {
		return 0, nil, err
	}
	f.Status = b.Status
	return http.StatusOK, fileOut(f), nil
}

// ---- events, notifications, mail ----

func (q *request) createEvent() (int, any, error) {
	var b struct {
		Action, RecordType, RecordID, MatterID, Summary string
		ClientVisible                                   bool
	}
	var raw struct {
		Action        string `json:"action"`
		RecordType    string `json:"record_type"`
		RecordID      string `json:"record_id"`
		MatterID      string `json:"matter_id"`
		Summary       string `json:"summary"`
		ClientVisible bool   `json:"client_visible"`
	}
	if err := q.decode(&raw); err != nil {
		return 0, nil, err
	}
	b.Action, b.RecordType, b.RecordID, b.MatterID, b.Summary, b.ClientVisible = raw.Action, raw.RecordType, raw.RecordID, raw.MatterID, raw.Summary, raw.ClientVisible
	q.c.Events = append(q.c.Events, Event{Action: b.Action, RecordType: b.RecordType, RecordID: b.RecordID, MatterID: b.MatterID, Summary: b.Summary,
		ClientVisible: b.ClientVisible})
	return http.StatusCreated, map[string]any{"id": q.c.newID("tev")}, nil
}

func (q *request) notify() (int, any, error) {
	var b struct {
		Kind     string `json:"kind"`
		Title    string `json:"title"`
		Link     string `json:"link"`
		Audience string `json:"audience"`
		MatterID string `json:"matter_id"`
	}
	if err := q.decode(&b); err != nil {
		return 0, nil, err
	}
	if b.Kind != "exception" && b.Kind != "refund_proposed" {
		return 0, nil, perr(http.StatusUnprocessableEntity, "validation_failed", "undeclared kind "+b.Kind)
	}
	if !strings.HasPrefix(b.Link, "/modules/"+ModuleID+"/") && !strings.HasPrefix(b.Link, "/portal/"+ModuleID+"/") {
		return 0, nil, perr(http.StatusUnprocessableEntity, "validation_failed", "link")
	}
	if b.Audience != "admins" && b.MatterID == "" {
		return 0, nil, perr(http.StatusUnprocessableEntity, "validation_failed", "matter_id")
	}
	q.c.Notifications = append(q.c.Notifications, Notification{Kind: ModuleID + "." + b.Kind, Title: b.Title, Link: b.Link, Audience: b.Audience, MatterID: b.MatterID})
	return http.StatusCreated, map[string]any{"created": 1}, nil
}

func (q *request) mail() (int, any, error) {
	if q.caller.kind != "viewer" || !q.staff() {
		return 0, nil, errScope
	}
	var b struct {
		MatterID string   `json:"matter_id"`
		To       []string `json:"to_contact_ids"`
		Subject  string   `json:"subject"`
		Text     string   `json:"text"`
		LinkPath string   `json:"link_path"`
	}
	if err := q.decode(&b); err != nil {
		return 0, nil, err
	}
	if !q.canMatter(b.MatterID) {
		return 0, nil, errNotFound
	}
	if q.c.failNext["mail_result"] > 0 {
		q.c.failNext["mail_result"]--
		return http.StatusOK, map[string]any{"results": []any{map[string]any{"contact_id": b.To[0], "status": "failed", "error": "mailbox unavailable"}}}, nil
	}
	q.c.Mails = append(q.c.Mails, Mail{MatterID: b.MatterID, To: b.To, Subject: b.Subject, Text: b.Text, LinkPath: b.LinkPath})
	var res []any
	for _, to := range b.To {
		res = append(res, map[string]any{"contact_id": to, "status": "sent"})
	}
	return http.StatusOK, map[string]any{"results": res}, nil
}

func (q *request) conflict() (int, any, error) {
	m, ok := q.c.conflicts[q.params["id"]]
	if !ok {
		return 0, nil, errNotFound
	}
	decision := "no_go"
	if strings.HasPrefix(m, "clear:") {
		m, decision = strings.TrimPrefix(m, "clear:"), "clear"
	}
	return http.StatusOK, map[string]any{"id": q.params["id"], "matter_id": m, "hits": []any{map[string]any{"id": "hit1", "decision": decision}}}, nil
}

func (q *request) booksProfile() (int, any, error) {
	if q.caller.kind != "service" {
		return 0, nil, errScope
	}
	return http.StatusOK, map[string]any{"firm_name": "Synthetic Law LLC", "time_zone": "America/Chicago", "currency": "USD"}, nil
}

func (q *request) booksMatter() (int, any, error) {
	if q.caller.kind != "service" || !q.canMatter(q.params["id"]) {
		return 0, nil, errNotFound
	}
	m := q.c.matters[q.params["id"]]
	return http.StatusOK, map[string]any{"id": m.ID, "number": m.Number, "name": m.Name, "status": m.Status, "client_contact_id": m.ClientContact,
		"responsible_attorney_user_id": m.ResponsibleAttorney}, nil
}

var _ = fmt.Sprint
