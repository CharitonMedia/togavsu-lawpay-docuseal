package lawpaydocuseal

import (
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/CharitonMedia/togavsu-lawpay-docuseal/adapters"
	"github.com/CharitonMedia/togavsu-lawpay-docuseal/fake"
)

// corePayment reads a payment through the core API as u.
func (e *env) corePayment(id string, u User) map[string]any {
	e.t.Helper()
	r := e.do(http.MethodGet, e.cored.URL+"/api/v1/payments/"+id, map[string]string{"Authorization": "Bearer " + e.core.Token,
		"X-Togavsu-Context-Token": u.Token}, nil)
	if r.status != http.StatusOK {
		e.t.Fatalf("payment %s: %d %s", id, r.status, r.body)
	}
	return r.json(e.t)
}

func (e *env) hook(body []byte) string {
	e.t.Helper()
	r := e.lawpayHook(e.secrets["lawpay_webhook_token"], body)
	if r.status != http.StatusOK {
		e.t.Fatalf("hook: %d %s", r.status, r.body)
	}
	return r.str(e.t, "verdict")
}

func (e *env) refund(u User, payment string, cents int64) resp {
	e.t.Helper()
	return e.api(http.MethodPost, "/api/refunds", u, map[string]any{"payment_id": payment, "amount_cents": cents})
}

func (e *env) openExceptions(kind string) []map[string]any {
	e.t.Helper()
	r := e.api(http.MethodGet, "/api/exceptions?kind="+kind, e.attorney, nil)
	var out []map[string]any
	for _, x := range r.json(e.t)["data"].([]any) {
		out = append(out, x.(map[string]any))
	}
	return out
}

// moneyState counts what a webhook must not change.
func (e *env) moneyState() string {
	e.t.Helper()
	ps := e.core.Payments()
	var b strings.Builder
	for _, p := range ps {
		b.WriteString(p.ID + p.Status + "|")
		for _, r := range e.core.RefundsOf(p.ID) {
			b.WriteString(r.ID + r.Status + ",")
		}
	}
	b.WriteString(e.text(`SELECT string_agg(id || state, ',' ORDER BY id) FROM lawpay_charges`))
	b.WriteString(e.text(`SELECT string_agg(id || state, ',' ORDER BY id) FROM lawpay_refunds`))
	return b.String()
}

// MOD-PAY-15: a missing or wrong token gets 401 and writes nothing.
func TestPGModPay15(t *testing.T) {
	e := newEnv(t)
	body := fake.ChargeWebhook(e.lp.AddCharge("acct_operating", "card", 1000))
	for _, tok := range []string{"", "wrong"} {
		if r := e.lawpayHook(tok, body); r.status != http.StatusUnauthorized {
			t.Fatalf("token %q: %d", tok, r.status)
		}
	}
	if n := e.count(`SELECT count(*) FROM lawpay_webhook_receipts`); n != 0 || e.lp.Calls() != 0 {
		t.Fatalf("receipts %d", n)
	}
}

// MOD-PAY-16: malformed JSON with a good token is stored as malformed and answers 200.
func TestPGModPay16(t *testing.T) {
	e := newEnv(t)
	if v := e.hook([]byte(`{"event": "transaction.completed", "data": {`)); v != "malformed" {
		t.Fatalf("verdict %s", v)
	}
	if got := e.text(`SELECT verdict FROM lawpay_webhook_receipts`); got != "malformed" || e.lp.Calls() != 0 {
		t.Fatalf("stored %s", got)
	}
}

// MOD-PAY-17: a duplicate delivery counts and changes nothing.
func TestPGModPay17(t *testing.T) {
	e := newEnv(t)
	_, _, lpID := e.staffCharge("trust", 40000, fake.BankToken)
	e.lp.Settle(lpID, "COMPLETED")
	tx, _ := e.lp.Get(lpID)
	body := fake.ChargeWebhook(tx)
	e.hook(body)
	before := e.moneyState()
	if v := e.hook(body); v != "duplicate" {
		t.Fatalf("verdict %s", v)
	}
	if n := e.count(`SELECT duplicate_count FROM lawpay_webhook_receipts`); n != 1 {
		t.Fatalf("duplicate_count %d", n)
	}
	if e.moneyState() != before || len(e.core.TrustEntries("deposit")) != 1 {
		t.Fatal("state changed")
	}
}

// MOD-PAY-18: a transaction LawPay doesn't know changes nothing and is unverified.
func TestPGModPay18(t *testing.T) {
	e := newEnv(t)
	e.staffCharge("operating", 5000, fake.CardToken)
	before := e.moneyState()
	body := fake.ChargeWebhook(adapters.Transaction{ID: "ch_9999", Type: "CHARGE", Status: "COMPLETED", AccountID: "acct_operating", AmountCents: 5000})
	if v := e.hook(body); v != "unverified" {
		t.Fatalf("verdict %s", v)
	}
	if e.moneyState() != before || e.count(`SELECT count(*) FROM exceptions`) != 0 {
		t.Fatal("state changed")
	}
}

// MOD-PAY-19: a refund-shaped body naming a pending charge leaves it pending.
func TestPGModPay19(t *testing.T) {
	e := newEnv(t)
	charge, pay, lpID := e.staffCharge("trust", 40000, fake.BankToken)
	// Its own id with the charge id at the top level: LawPay says CHARGE, the body says
	// REFUND, so it fails closed. And a refund id LawPay doesn't know.
	for _, id := range []string{lpID, "rf_9999"} {
		body := fake.RefundWebhook(adapters.Transaction{ID: id, Status: "COMPLETED", ChargeID: lpID, AccountID: "acct_trust", AmountCents: 40000})
		e.hook(body)
	}
	if p := e.core.PaymentRecord(pay); p.Status != "pending" {
		t.Fatalf("payment %s", p.Status)
	}
	if s := e.text(`SELECT state FROM lawpay_charges WHERE id = $1`, charge); s != "recorded_pending" || len(e.core.TrustEntries("")) != 0 {
		t.Fatalf("charge %s", s)
	}
}

// MOD-PAY-20: an account that matches neither setting, or disagrees with the row, posts
// nothing and opens account_mismatch.
func TestPGModPay20(t *testing.T) {
	e := newEnv(t)
	e.hook(fake.ChargeWebhook(e.lp.AddCharge("acct_elsewhere", "card", 3000)))
	_, pay, lpID := e.staffCharge("trust", 40000, fake.BankToken)
	e.lp.Update(lpID, func(tx *adapters.Transaction) { tx.AccountID, tx.Status = "acct_operating", "COMPLETED" })
	tx, _ := e.lp.Get(lpID)
	e.hook(fake.ChargeWebhook(tx))
	if n := len(e.openExceptions("account_mismatch")); n != 2 {
		t.Fatalf("exceptions %d", n)
	}
	if e.core.PaymentRecord(pay).Status != "pending" || len(e.core.Payments()) != 1 || len(e.core.TrustEntries("")) != 0 {
		t.Fatal("posted")
	}
}

// MOD-PAY-21: an unknown verified charge opens unknown_transaction; an attorney records it.
func TestPGModPay21(t *testing.T) {
	e := newEnv(t)
	tx := e.lp.AddCharge("acct_trust", "bank", 12345)
	e.lp.Update(tx.ID, func(t *adapters.Transaction) { t.Status = "COMPLETED" })
	if v := e.hook(fake.ChargeWebhook(tx)); v != "unknown_transaction" {
		t.Fatalf("verdict %s", v)
	}
	ex := e.openExceptions("unknown_transaction")
	if len(ex) != 1 || ex[0]["amount_cents"].(float64) != 12345 {
		t.Fatalf("exceptions %+v", ex)
	}
	// Staff can't record it.
	id := ex[0]["id"].(string)
	if r := e.api(http.MethodPost, "/api/exceptions/"+id+"/record", e.staff, map[string]string{"matter_id": e.matter.ID, "contact_id": e.clientContact.ID}); r.status != http.StatusForbidden {
		t.Fatalf("staff: %d", r.status)
	}
	r := e.form("/tab/record", e.attorney, url.Values{"exception_id": {id}, "matter_id": {e.matter.ID}, "contact_id": {e.clientContact.ID}})
	if r.status != http.StatusOK {
		t.Fatalf("record: %d %s", r.status, r.body)
	}
	ps := e.core.Payments()
	if len(ps) != 1 || ps[0].AmountCents != 12345 || ps[0].Method != "ach" || ps[0].Destination != "trust" || ps[0].Reference != tx.ID ||
		ps[0].MatterID != e.matter.ID || ps[0].TrustAccountID != e.trustAccount {
		t.Fatalf("payment %+v", ps)
	}
	if len(e.openExceptions("unknown_transaction")) != 0 {
		t.Fatal("still open")
	}
}

// MOD-PAY-22: GET /hooks/lawpay is a health check; the public route takes POST only.
func TestPGModPay22(t *testing.T) {
	e := newEnv(t)
	if r := e.do(http.MethodGet, e.mod.URL+"/hooks/lawpay", nil, nil); r.status != http.StatusOK || strings.TrimSpace(string(r.body)) != `{"ok":true}` {
		t.Fatalf("module GET: %d %s", r.status, r.body)
	}
	if r := e.do(http.MethodGet, e.cored.URL+"/hooks/lawpay-docuseal/lawpay?token="+e.secrets["lawpay_webhook_token"], nil, nil); r.status != http.StatusMethodNotAllowed {
		t.Fatalf("public GET: %d", r.status)
	}
}

// MOD-PAY-23 and 24: a partial refund in order, then the rest, then a cent too many.
func TestPGModPay23And24(t *testing.T) {
	e := newEnv(t)
	inv := e.core.AddInvoice(e.matter.ID, 10000)
	in, res := e.pay(e.staff, map[string]any{"matter_id": e.matter.ID, "amount_cents": 10000, "invoice_id": inv.ID}, fake.CardToken)
	pay := res.str(t, "payment_id")
	if in.status != http.StatusCreated || e.core.InvoiceRecord(inv.ID).PaidCents != 10000 {
		t.Fatalf("charge %s", res.body)
	}
	mark := len(e.core.Calls)
	r := e.refund(e.attorney, pay, 4000)
	if r.status != http.StatusOK || r.str(t, "state") != "posted" {
		t.Fatalf("refund: %d %s", r.status, r.body)
	}
	var order []string
	for _, c := range e.core.Calls[mark:] {
		if c.Op == "createPaymentRefund" || c.Op == "updatePaymentRefundStatus" {
			order = append(order, c.Op)
		}
	}
	if strings.Join(order, ",") != "createPaymentRefund,updatePaymentRefundStatus" || e.lp.RefundCalls != 1 {
		t.Fatalf("order %v, refunds %d", order, e.lp.RefundCalls)
	}
	refs := e.core.RefundsOf(pay)
	if len(refs) != 1 || refs[0].Status != "completed" || refs[0].Reference != r.str(t, "lawpay_refund_id") {
		t.Fatalf("core refunds %+v", refs)
	}
	p := e.corePayment(pay, e.attorney)
	if p["refunded_cents"].(float64) != 4000 || p["refund_state"] != "partial" || e.core.InvoiceRecord(inv.ID).PaidCents != 6000 {
		t.Fatalf("payment %+v invoice %d", p, e.core.InvoiceRecord(inv.ID).PaidCents)
	}
	if s := e.text(`SELECT state FROM lawpay_refunds`); s != "posted" {
		t.Fatalf("row %s", s)
	}
	// MOD-PAY-24.
	if r := e.refund(e.attorney, pay, 6000); r.str(t, "state") != "posted" {
		t.Fatalf("second: %s", r.body)
	}
	if p := e.corePayment(pay, e.attorney); p["refund_state"] != "full" {
		t.Fatalf("payment %+v", p)
	}
	calls := e.lp.RefundCalls
	if r := e.refund(e.attorney, pay, 1); r.status != http.StatusUnprocessableEntity || r.str(t, "code") != "refund_exceeds_payment" {
		t.Fatalf("module: %d %s", r.status, r.body)
	}
	core := e.do(http.MethodPost, e.cored.URL+"/api/v1/payments/"+pay+"/refunds", map[string]string{"Authorization": "Bearer " + e.core.Token,
		"X-Togavsu-Context-Token": e.attorney.Token, "Content-Type": "application/json"}, []byte(`{"amount_cents":1,"status":"pending"}`))
	if core.status != http.StatusUnprocessableEntity || !strings.Contains(string(core.body), "refund_exceeds_payment") || e.lp.RefundCalls != calls {
		t.Fatalf("core: %d %s", core.status, core.body)
	}
}

// MOD-PAY-25: a refund above the payment is refused before any write or adapter call.
func TestPGModPay25(t *testing.T) {
	e := newEnv(t)
	_, pay, _ := e.staffCharge("operating", 10000, fake.CardToken)
	writes := e.core.Writes()
	if r := e.refund(e.attorney, pay, 15000); r.status != http.StatusUnprocessableEntity || r.str(t, "code") != "refund_exceeds_payment" {
		t.Fatalf("%d %s", r.status, r.body)
	}
	if e.core.Writes() != writes || e.lp.RefundCalls != 0 || e.count(`SELECT count(*) FROM lawpay_refunds`) != 0 {
		t.Fatal("written")
	}
}

// MOD-PAY-26: a trust refund that takes the client below zero warns and still posts.
func TestPGModPay26(t *testing.T) {
	e := newEnv(t)
	_, pay, _ := e.staffCharge("trust", 30000, fake.CardToken)
	e.core.AddTrustEntry(e.matter.ID, e.clientContact.ID, e.trustAccount, "disbursement", -20000)
	want := "This leaves the client at -$200.00 in trust. It will post and raise a trust flag."
	page := e.page("/matter/refund", e.attorney, url.Values{"payment_id": {pay}})
	if !strings.Contains(html.UnescapeString(string(page.body)), want) || !strings.Contains(string(page.body), `<button type="submit">Refund</button>`) {
		t.Fatalf("panel %s", page.body)
	}
	r := e.form("/matter/refund", e.attorney, url.Values{"payment_id": {pay}, "amount": {"300.00"}})
	if r.status != http.StatusOK || !strings.Contains(string(r.body), "Refund posted") {
		t.Fatalf("refund %d %s", r.status, r.body)
	}
	if rows := e.core.TrustEntries("refund"); len(rows) != 1 || rows[0].Cents != -30000 {
		t.Fatalf("refund rows %+v", rows)
	}
	notes, mails, _ := e.core.Snapshot()
	flags := 0
	for _, n := range notes {
		if n.Kind == "trust.flag_raised" {
			flags++
		}
	}
	if len(e.core.TrustFlags) != 1 || flags != 1 || len(mails) != 1 || strings.Join(mails[0].To, ",") != "admins,responsible_attorney" {
		t.Fatalf("flags %d, notes %+v, mails %+v", len(e.core.TrustFlags), notes, mails)
	}
}

// MOD-PAY-27: staff and accountants see no Refund, and the core forbids a direct call.
func TestPGModPay27(t *testing.T) {
	e := newEnv(t)
	_, pay, _ := e.staffCharge("operating", 10000, fake.CardToken)
	for _, u := range []User{e.staff, e.accountant} {
		tab := e.page("/matter", u, url.Values{"matter_id": {e.matter.ID}})
		if tab.status != http.StatusOK || strings.Contains(string(tab.body), "matter/refund") {
			t.Fatalf("matter tab %d %s", tab.status, tab.body)
		}
		form := e.page("/matter/refund", u, url.Values{"payment_id": {pay}})
		if strings.Contains(string(form.body), `name="amount"`) {
			t.Fatalf("refund form shown: %s", form.body)
		}
		if r := e.refund(u, pay, 1000); r.status != http.StatusForbidden {
			t.Fatalf("direct: %d %s", r.status, r.body)
		}
	}
	if tab := e.page("/matter", e.attorney, url.Values{"matter_id": {e.matter.ID}}); !strings.Contains(string(tab.body), "matter/refund") {
		t.Fatalf("attorney tab %s", tab.body)
	}
	if e.lp.RefundCalls != 0 || len(e.core.RefundsOf(pay)) != 0 {
		t.Fatal("refunded")
	}
}

// MOD-PAY-28: an adapter refund failure marks both failed and shows the LawPay message.
func TestPGModPay28(t *testing.T) {
	e := newEnv(t)
	_, pay, _ := e.staffCharge("trust", 10000, fake.CardToken)
	e.lp.Script("refund_fail")
	r := e.refund(e.attorney, pay, 5000)
	if r.status != http.StatusBadGateway || !strings.Contains(r.str(t, "message"), "The refund could not be processed.") {
		t.Fatalf("%d %s", r.status, r.body)
	}
	refs := e.core.RefundsOf(pay)
	if len(refs) != 1 || refs[0].Status != "failed" || e.text(`SELECT state FROM lawpay_refunds`) != "failed" {
		t.Fatalf("refunds %+v", refs)
	}
	if len(e.core.TrustEntries("refund")) != 0 || e.core.PaymentRecord(pay).RefundedCents != 0 {
		t.Fatal("posted")
	}
}

// MOD-PAY-29: LawPay refunds, the core completion fails, the sweep posts it with the
// grant, and the later webhook does nothing.
func TestPGModPay29(t *testing.T) {
	e := newEnv(t)
	_, pay, _ := e.staffCharge("operating", 10000, fake.CardToken)
	e.core.FailNext("updatePaymentRefundStatus", 1)
	r := e.refund(e.attorney, pay, 2500)
	if r.str(t, "state") != "processor_done" {
		t.Fatalf("%s", r.body)
	}
	e.srv.Sweep(e.ctx)
	calls := e.core.CallsOf("updatePaymentRefundStatus")
	if len(calls) != 2 || calls[1].Kind != "task" {
		t.Fatalf("calls %+v", calls)
	}
	if refs := e.core.RefundsOf(pay); len(refs) != 1 || refs[0].Status != "completed" || e.text(`SELECT state FROM lawpay_refunds`) != "posted" {
		t.Fatalf("refunds %+v", refs)
	}
	tx, _ := e.lp.Get(r.str(t, "lawpay_refund_id"))
	before := e.moneyState()
	if v := e.hook(fake.RefundWebhook(tx)); v != "applied" || e.moneyState() != before || e.count(`SELECT count(*) FROM exceptions`) != 0 {
		t.Fatalf("webhook %s", v)
	}
}

// MOD-PAY-30: dashboard refunds open external_refund; Post refund records them.
func TestPGModPay30(t *testing.T) {
	e := newEnv(t)
	_, pay, lpID := e.staffCharge("operating", 10000, fake.CardToken)
	_, pay2, lpID2 := e.staffCharge("operating", 8000, fake.CardToken)
	full := e.lp.DashboardRefund(lpID, 10000)
	part := e.lp.DashboardRefund(lpID2, 3000)
	e.hook(fake.RefundWebhook(full))
	e.hook(fake.RefundWebhook(part))
	ex := e.openExceptions("external_refund")
	if len(ex) != 2 || ex[0]["amount_cents"].(float64) != 10000 {
		t.Fatalf("exceptions %+v", ex)
	}
	if notes, _, _ := e.core.Snapshot(); len(notes) != 2 {
		t.Fatalf("notifications %+v", notes)
	}
	if r := e.api(http.MethodPost, "/api/exceptions/"+ex[0]["id"].(string)+"/post-refund", e.staff, nil); r.status != http.StatusForbidden {
		t.Fatalf("staff: %d", r.status)
	}
	for _, x := range ex {
		r := e.form("/tab/post-refund", e.attorney, url.Values{"exception_id": {x["id"].(string)}})
		if r.status != http.StatusOK {
			t.Fatalf("post: %d %s", r.status, r.body)
		}
	}
	if refs := e.core.RefundsOf(pay); len(refs) != 1 || refs[0].Status != "completed" || refs[0].Reference != full.ID {
		t.Fatalf("refunds %+v", refs)
	}
	if p := e.corePayment(pay, e.attorney); p["refund_state"] != "full" {
		t.Fatalf("payment %+v", p)
	}
	if p := e.corePayment(pay2, e.attorney); p["refund_state"] != "partial" || p["refunded_cents"].(float64) != 3000 {
		t.Fatalf("payment %+v", p)
	}
	if len(e.openExceptions("external_refund")) != 0 || e.lp.RefundCalls != 0 {
		t.Fatal("still open or refunded again")
	}
}

// MOD-PAY-31: a no-go conflict check proposes the consult fee refund once.
func TestPGModPay31(t *testing.T) {
	e := newEnv(t)
	_, pay, _ := e.staffCharge("operating", 17500, fake.CardToken)
	cc := e.core.AddConflictCheck(e.matter.ID, true)
	e.core.EmitWithID("evt_nogo", "conflict_check.decided", "conflict_check", cc, e.matter.ID)
	e.core.EmitWithID("evt_nogo", "conflict_check.decided", "conflict_check", cc, e.matter.ID)
	e.core.Emit("conflict_check.decided", "conflict_check", cc, e.matter.ID, nil)
	if n := e.count(`SELECT count(*) FROM lawpay_refunds WHERE state = 'proposed' AND amount_cents = 17500 AND payment_id = $1`, pay); n != 1 {
		t.Fatalf("proposed %d", n)
	}
	notes, _, _ := e.core.Snapshot()
	if len(notes) != 1 || notes[0].Kind != "lawpay-docuseal.refund_proposed" || notes[0].Audience != "responsible_attorney" {
		t.Fatalf("notifications %+v", notes)
	}
	tab := e.page("/matter", e.attorney, url.Values{"matter_id": {e.matter.ID}})
	if !strings.Contains(string(tab.body), "Issue refund") {
		t.Fatalf("tab %s", tab.body)
	}
	id := e.text(`SELECT id FROM lawpay_refunds`)
	if r := e.form("/matter/issue", e.attorney, url.Values{"refund_id": {id}}); r.status != http.StatusOK {
		t.Fatalf("issue %d %s", r.status, r.body)
	}
	if refs := e.core.RefundsOf(pay); len(refs) != 1 || refs[0].Status != "completed" || refs[0].AmountCents != 17500 || e.lp.RefundCalls != 1 {
		t.Fatalf("refunds %+v", refs)
	}
	// A matter whose latest payment wasn't through LawPay.
	_, other := e.newClient("Morgan")
	e.core.AddPayment(other.ID, other.ClientContact, "check", 9000, e.clock.Now())
	cc2 := e.core.AddConflictCheck(other.ID, true)
	e.core.Emit("conflict_check.decided", "conflict_check", cc2, other.ID, nil)
	if ex := e.openExceptions("manual_refund_needed"); len(ex) != 1 || ex[0]["matter_id"] != other.ID {
		t.Fatalf("manual %+v", ex)
	}
	// A go decision does nothing.
	cc3 := e.core.AddConflictCheck(e.matter.ID, false)
	e.core.Emit("conflict_check.decided", "conflict_check", cc3, e.matter.ID, nil)
	if n := e.count(`SELECT count(*) FROM lawpay_refunds`); n != 1 {
		t.Fatalf("refunds %d", n)
	}
}

// MOD-PAY-32: the sweep advances young pending ACH charges and flags old ones.
func TestPGModPay32(t *testing.T) {
	e := newEnv(t)
	old, oldPay, _ := e.staffCharge("trust", 20000, fake.BankToken)
	e.clock.Advance(15 * 24 * time.Hour)
	young, youngPay, youngLP := e.staffCharge("trust", 30000, fake.BankToken)
	e.lp.Settle(youngLP, "COMPLETED")
	reads := e.lp.ReadCalls
	e.srv.Sweep(e.ctx)
	if e.lp.ReadCalls-reads != 1 {
		t.Fatalf("reads %d", e.lp.ReadCalls-reads)
	}
	if s := e.text(`SELECT state FROM lawpay_charges WHERE id = $1`, young); s != "recorded_completed" || e.core.PaymentRecord(youngPay).Status != "completed" {
		t.Fatalf("young %s", s)
	}
	if s := e.text(`SELECT state FROM lawpay_charges WHERE id = $1`, old); s != "recorded_pending" || e.core.PaymentRecord(oldPay).Status != "pending" {
		t.Fatalf("old %s", s)
	}
	if ex := e.openExceptions("pending_ach_stale"); len(ex) != 1 || ex[0]["charge_id"] != old {
		t.Fatalf("exceptions %+v", ex)
	}
	e.srv.Sweep(e.ctx)
	if len(e.openExceptions("pending_ach_stale")) != 1 {
		t.Fatal("opened twice")
	}
}

// MOD-PAY-33: the reconciliation tab lists and filters; Resolve needs a note.
func TestPGModPay33(t *testing.T) {
	e := newEnv(t)
	_, pay, _ := e.staffCharge("operating", 10000, fake.CardToken)
	e.staffCharge("trust", 20000, fake.BankToken)
	e.refund(e.attorney, pay, 1000)
	e.hook(fake.ChargeWebhook(e.lp.AddCharge("acct_operating", "card", 777)))
	all := e.api(http.MethodGet, "/api/transactions", e.staff, nil).json(t)["data"].([]any)
	if len(all) != 3 {
		t.Fatalf("all %d", len(all))
	}
	first := all[0].(map[string]any)
	if first["matter_id"] != e.matter.ID || first["amount_cents"].(float64) != 10000 || first["destination"] != "operating" ||
		first["state"] != "recorded_completed" || first["payment_link"] != "/money/payments/"+pay {
		t.Fatalf("row %+v", first)
	}
	if n := len(e.api(http.MethodGet, "/api/transactions?state=recorded_pending", e.staff, nil).json(t)["data"].([]any)); n != 1 {
		t.Fatalf("pending %d", n)
	}
	if n := len(e.api(http.MethodGet, "/api/transactions?kind=refund", e.staff, nil).json(t)["data"].([]any)); n != 1 {
		t.Fatalf("refunds %d", n)
	}
	tab := e.page("/tab/reconciliation", e.attorney, url.Values{"kind": {"charge"}})
	if tab.status != http.StatusOK || !strings.Contains(string(tab.body), "/money/payments/"+pay) || !strings.Contains(string(tab.body), "Resolve") {
		t.Fatalf("tab %s", tab.body)
	}
	ex := e.openExceptions("")
	if len(ex) != 1 {
		t.Fatalf("exceptions %+v", ex)
	}
	id := ex[0]["id"].(string)
	if r := e.form("/tab/resolve", e.attorney, url.Values{"exception_id": {id}, "note": {"  "}}); r.status != http.StatusUnprocessableEntity {
		t.Fatalf("no note: %d", r.status)
	}
	if r := e.form("/tab/resolve", e.attorney, url.Values{"exception_id": {id}, "note": {"A test charge in the LawPay dashboard."}}); r.status != http.StatusOK {
		t.Fatalf("resolve: %d %s", r.status, r.body)
	}
	if len(e.openExceptions("")) != 0 || e.text(`SELECT resolution_note FROM exceptions`) != "A test charge in the LawPay dashboard." {
		t.Fatal("not resolved")
	}
}
