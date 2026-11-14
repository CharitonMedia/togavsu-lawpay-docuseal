package acceptance

import (
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/CharitonMedia/togavsu/pkg/modulekit/coretest"

	"github.com/CharitonMedia/togavsu-lawpay-docuseal/adapters"
	"github.com/CharitonMedia/togavsu-lawpay-docuseal/fake"
)

// MOD-PAY-15: a missing or wrong token gets 401 and writes nothing, not even a receipt.
func TestCoreModPay15(t *testing.T) {
	e := newCoreEnv(t, "CORE-07")
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
func TestCoreModPay16(t *testing.T) {
	e := newCoreEnv(t, "CORE-07")
	if v := e.hook([]byte(`{"event": "transaction.completed", "data": {`)); v != "malformed" {
		t.Fatalf("verdict %s", v)
	}
	if got := e.text(`SELECT verdict FROM lawpay_webhook_receipts`); got != "malformed" || e.lp.Calls() != 0 {
		t.Fatalf("stored %s", got)
	}
}

// MOD-PAY-17: a duplicate delivery counts and changes nothing.
func TestCoreModPay17(t *testing.T) {
	e := newCoreEnv(t, "CORE-07", "CORE-23")
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
	if e.moneyState() != before || e.trustRows("deposit") != 1 {
		t.Fatal("state changed")
	}
}

// MOD-PAY-18: a transaction LawPay doesn't know changes nothing and is unverified.
func TestCoreModPay18(t *testing.T) {
	e := newCoreEnv(t, "CORE-07", "CORE-23")
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

// MOD-PAY-19: a refund-shaped body naming a pending charge leaves it and its core
// payment pending.
func TestCoreModPay19(t *testing.T) {
	e := newCoreEnv(t, "CORE-07", "CORE-23")
	charge, pay, lpID := e.staffCharge("trust", 40000, fake.BankToken)
	for _, id := range []string{lpID, "rf_9999"} {
		e.hook(fake.RefundWebhook(adapters.Transaction{ID: id, Status: "COMPLETED", ChargeID: lpID, AccountID: "acct_trust", AmountCents: 40000}))
	}
	if p := e.payment(pay); p["status"] != "pending" {
		t.Fatalf("payment %+v", p)
	}
	if s := e.text(`SELECT state FROM lawpay_charges WHERE id = $1`, charge); s != "recorded_pending" || e.trustRows("") != 0 {
		t.Fatalf("charge %s", s)
	}
}

// MOD-PAY-20: an account that matches neither setting, or disagrees with the row,
// posts nothing and opens account_mismatch.
func TestCoreModPay20(t *testing.T) {
	e := newCoreEnv(t, "CORE-07", "CORE-23")
	e.hook(fake.ChargeWebhook(e.lp.AddCharge("acct_elsewhere", "card", 3000)))
	_, pay, lpID := e.staffCharge("trust", 40000, fake.BankToken)
	e.lp.Update(lpID, func(tx *adapters.Transaction) { tx.AccountID, tx.Status = "acct_operating", "COMPLETED" })
	tx, _ := e.lp.Get(lpID)
	e.hook(fake.ChargeWebhook(tx))
	if n := len(e.openExceptions("account_mismatch")); n != 2 {
		t.Fatalf("exceptions %d", n)
	}
	if e.payment(pay)["status"] != "pending" || len(e.payments()) != 1 || e.trustRows("") != 0 {
		t.Fatal("posted")
	}
}

// MOD-PAY-21: a verified charge with no charge row opens unknown_transaction; an
// attorney records it with the verified amount, method and destination.
func TestCoreModPay21(t *testing.T) {
	e := newCoreEnv(t, "CORE-07", "CORE-23")
	tx := e.lp.AddCharge("acct_trust", "bank", 12345)
	e.lp.Update(tx.ID, func(t *adapters.Transaction) { t.Status = "COMPLETED" })
	if v := e.hook(fake.ChargeWebhook(tx)); v != "unknown_transaction" {
		t.Fatalf("verdict %s", v)
	}
	ex := e.openExceptions("unknown_transaction")
	if len(ex) != 1 || ex[0]["amount_cents"].(float64) != 12345 {
		t.Fatalf("exceptions %+v", ex)
	}
	id := ex[0]["id"].(string)
	if r := e.api(http.MethodPost, "/api/exceptions/"+id+"/record", coretest.Staff, map[string]string{"matter_id": e.matter, "contact_id": e.client}); r.status != http.StatusForbidden {
		t.Fatalf("staff: %d", r.status)
	}
	if r := e.form("/tab/record", coretest.Attorney, url.Values{"exception_id": {id}, "matter_id": {e.matter}, "contact_id": {e.client}}); r.status != http.StatusOK {
		t.Fatalf("record: %d %s", r.status, r.body)
	}
	ps := e.payments()
	if len(ps) != 1 || ps[0]["amount_cents"] != 12345.0 || ps[0]["method"] != "ach" || ps[0]["destination"] != "trust" ||
		ps[0]["reference_number"] != tx.ID || ps[0]["matter_id"] != e.matter || ps[0]["trust_account_id"] != e.trustAccount {
		t.Fatalf("payment %+v", ps)
	}
	if len(e.openExceptions("unknown_transaction")) != 0 {
		t.Fatal("still open")
	}
}

// MOD-PAY-22: the module's GET /hooks/lawpay is a health check; the core's public route
// takes POST only.
func TestCoreModPay22(t *testing.T) {
	e := newCoreEnv(t, "CORE-07")
	if r := e.send(http.MethodGet, e.modURL()+"/hooks/lawpay", nil, nil); r.status != http.StatusOK || strings.TrimSpace(string(r.body)) != `{"ok":true}` {
		t.Fatalf("module GET: %d %s", r.status, r.body)
	}
	if r := e.send(http.MethodGet, e.c.URL+"/hooks/"+modID+"/lawpay?token="+e.secrets["lawpay_webhook_token"], nil, nil); r.status != http.StatusMethodNotAllowed {
		t.Fatalf("public GET: %d", r.status)
	}
}

// MOD-PAY-23 and 24: a partial refund in order, then the rest, then a cent too many,
// refused by the module and by the core.
func TestCoreModPay23And24(t *testing.T) {
	e := newCoreEnv(t, "CORE-22", "CORE-23", "CORE-24")
	inv, total := e.invoice(e.matter, 5)
	e.setting("invoice_pay_destination", "operating")
	_, res := e.pay(coretest.Staff, map[string]any{"matter_id": e.matter, "amount_cents": total, "invoice_id": inv}, fake.CardToken)
	pay := res.str(t, "payment_id")
	if e.invoiceCents(inv, "amount_paid_cents") != total {
		t.Fatalf("charge %s", res.body)
	}
	part := total * 2 / 5
	mark := len(e.tr.of(http.MethodPost, "/api/v1/payments/"+pay+"/refunds"))
	r := e.refund(coretest.Attorney, pay, part)
	if r.status != http.StatusOK || r.str(t, "state") != "posted" {
		t.Fatalf("refund: %d %s", r.status, r.body)
	}
	var order []string
	for _, c := range e.tr.of(http.MethodPost, "/api/v1/payments/"+pay+"/refunds")[mark:] {
		if strings.HasSuffix(c.Path, "/status") {
			order = append(order, "status")
		} else {
			order = append(order, "create")
		}
	}
	if strings.Join(order, ",") != "create,status" || e.lp.RefundCalls != 1 {
		t.Fatalf("order %v, refunds %d", order, e.lp.RefundCalls)
	}
	if !e.refundWithRef(pay, r.str(t, "lawpay_refund_id"), part) || e.refundRows(pay, "") != 1 {
		t.Fatal("core refund not completed with the LawPay refund id")
	}
	p := e.payment(pay)
	if p["refunded_cents"] != float64(part) || p["refund_state"] != "partial" || e.invoiceCents(inv, "amount_paid_cents") != total-part {
		t.Fatalf("payment %+v invoice paid %d", p, e.invoiceCents(inv, "amount_paid_cents"))
	}
	if s := e.text(`SELECT state FROM lawpay_refunds`); s != "posted" {
		t.Fatalf("row %s", s)
	}
	// MOD-PAY-24.
	if r := e.refund(coretest.Attorney, pay, total-part); r.str(t, "state") != "posted" {
		t.Fatalf("second: %s", r.body)
	}
	if p := e.payment(pay); p["refund_state"] != "full" {
		t.Fatalf("payment %+v", p)
	}
	calls := e.lp.RefundCalls
	if r := e.refund(coretest.Attorney, pay, 1); r.status != http.StatusUnprocessableEntity || r.str(t, "code") != "refund_exceeds_payment" {
		t.Fatalf("module: %d %s", r.status, r.body)
	}
	core := e.c.ModuleClient(modID).Do(http.MethodPost, "/api/v1/payments/"+pay+"/refunds", map[string]any{"amount_cents": 1, "status": "pending"},
		coretest.ContextHeader, e.ctxFor(coretest.Attorney))
	if core.Code != http.StatusUnprocessableEntity || !strings.Contains(core.Body, "refund_exceeds_payment") || e.lp.RefundCalls != calls {
		t.Fatalf("core: %d %s", core.Code, core.Body)
	}
}

// MOD-PAY-25: a refund above the payment is refused before any write or adapter call.
func TestCoreModPay25(t *testing.T) {
	e := newCoreEnv(t, "CORE-23", "CORE-24")
	_, pay, _ := e.staffCharge("operating", 10000, fake.CardToken)
	writes := e.tr.writes()
	if r := e.refund(coretest.Attorney, pay, 15000); r.status != http.StatusUnprocessableEntity || r.str(t, "code") != "refund_exceeds_payment" {
		t.Fatalf("%d %s", r.status, r.body)
	}
	if e.tr.writes() != writes || e.lp.RefundCalls != 0 || e.count(`SELECT count(*) FROM lawpay_refunds`) != 0 {
		t.Fatal("written")
	}
}

// MOD-PAY-26: a trust refund that takes the client below zero warns, still posts, and
// the core raises one trust flag and tells the admins and the responsible attorney.
func TestCoreModPay26(t *testing.T) {
	e := newCoreEnv(t, "CORE-03", "CORE-23", "CORE-24")
	_, pay, _ := e.staffCharge("trust", 30000, fake.CardToken)
	e.c.Attorney.Must(http.StatusCreated, http.MethodPost, "/api/v1/trust-ledger", map[string]any{"trust_account_id": e.trustAccount,
		"client_id": e.client, "matter_id": e.matter, "entry_type": "withdrawal", "amount_cents": 20000, "method": "check", "description": "Synthetic disbursement"})
	want := "This leaves the client at -$200.00 in trust. It will post and raise a trust flag."
	page := e.page("/matter/refund", coretest.Attorney, url.Values{"payment_id": {pay}})
	if !strings.Contains(html.UnescapeString(string(page.body)), want) || !strings.Contains(string(page.body), `<button type="submit">Refund</button>`) {
		t.Fatalf("panel %s", page.body)
	}
	r := e.form("/matter/refund", coretest.Attorney, url.Values{"payment_id": {pay}, "amount": {"300.00"}})
	if r.status != http.StatusOK || !strings.Contains(string(r.body), "Refund posted") {
		t.Fatalf("refund %d %s", r.status, r.body)
	}
	if n := e.c.Count(`SELECT count(*) FROM core.trust_ledger WHERE entry_type = 'refund' AND amount_cents = 30000`); n != 1 {
		t.Fatalf("refund rows %d", n)
	}
	if n := e.c.Count(`SELECT count(*) FROM core.trust_flags WHERE matter_id = $1`, e.matter); n != 1 {
		t.Fatalf("trust flags %d", n)
	}
	if got := e.noteUsers("trust.flag_raised"); !got[coretest.Admin] || !got[coretest.Attorney] {
		t.Fatalf("flag notified %v", got)
	}
}

// MOD-PAY-27: staff and accountants see no Refund, and the core forbids a direct call.
func TestCoreModPay27(t *testing.T) {
	e := newCoreEnv(t, "CORE-23", "CORE-24")
	_, pay, _ := e.staffCharge("operating", 10000, fake.CardToken)
	for _, who := range []string{coretest.Staff, coretest.Accountant} {
		tab := e.page("/matter", who, url.Values{"matter_id": {e.matter}})
		if tab.status != http.StatusOK || strings.Contains(string(tab.body), "matter/refund") {
			t.Fatalf("%s matter tab %d %s", who, tab.status, tab.body)
		}
		if form := e.page("/matter/refund", who, url.Values{"payment_id": {pay}}); strings.Contains(string(form.body), `name="amount"`) {
			t.Fatalf("%s refund form shown: %s", who, form.body)
		}
		if r := e.refund(who, pay, 1000); r.status != http.StatusForbidden {
			t.Fatalf("%s direct: %d %s", who, r.status, r.body)
		}
		core := e.c.ModuleClient(modID).Do(http.MethodPost, "/api/v1/payments/"+pay+"/refunds", map[string]any{"amount_cents": 1000, "status": "pending"},
			coretest.ContextHeader, e.ctxFor(who))
		if core.Code != http.StatusForbidden {
			t.Fatalf("%s through the core: %d %s", who, core.Code, core.Body)
		}
	}
	if tab := e.page("/matter", coretest.Attorney, url.Values{"matter_id": {e.matter}}); !strings.Contains(string(tab.body), "matter/refund") {
		t.Fatalf("attorney tab %s", tab.body)
	}
	if e.lp.RefundCalls != 0 || e.refundRows(pay, "") != 0 {
		t.Fatal("refunded")
	}
}

// MOD-PAY-28: an adapter refund failure marks the core refund and the module row
// failed and shows the LawPay message; nothing posts.
func TestCoreModPay28(t *testing.T) {
	e := newCoreEnv(t, "CORE-23", "CORE-24")
	_, pay, _ := e.staffCharge("trust", 10000, fake.CardToken)
	e.lp.Script("refund_fail")
	r := e.refund(coretest.Attorney, pay, 5000)
	if r.status != http.StatusBadGateway || !strings.Contains(r.str(t, "message"), "The refund could not be processed.") {
		t.Fatalf("%d %s", r.status, r.body)
	}
	if e.refundRows(pay, "failed") != 1 || e.refundRows(pay, "") != 1 || e.text(`SELECT state FROM lawpay_refunds`) != "failed" {
		t.Fatal("refund not failed on both sides")
	}
	if e.trustRows("refund") != 0 || e.payment(pay)["refunded_cents"] != 0.0 {
		t.Fatal("posted")
	}
}

// MOD-PAY-29: LawPay refunds, the core completion fails, the sweep posts it with the
// refund grant, and the later webhook matches the stored refund id and does nothing.
func TestCoreModPay29(t *testing.T) {
	e := newCoreEnv(t, "CORE-07", "CORE-23", "CORE-24")
	_, pay, _ := e.staffCharge("operating", 10000, fake.CardToken)
	e.tr.failNextLike(http.MethodPost, "/api/v1/payments/"+pay+"/refunds/", "/status", 1)
	r := e.refund(coretest.Attorney, pay, 2500)
	if r.str(t, "state") != "processor_done" {
		t.Fatalf("%s", r.body)
	}
	if _, err := e.server().Sweep(e.ctx); err != nil {
		t.Fatal(err)
	}
	var status []coreCall
	for _, c := range e.tr.of(http.MethodPost, "/api/v1/payments/"+pay+"/refunds/") {
		if strings.HasSuffix(c.Path, "/status") {
			status = append(status, c)
		}
	}
	if len(status) != 2 || status[1].Header.Get("X-Togavsu-Task-Authorization") == "" || status[1].Header.Get(coretest.ContextHeader) != "" {
		t.Fatalf("status calls %d", len(status))
	}
	if !e.refundWithRef(pay, r.str(t, "lawpay_refund_id"), 2500) || e.text(`SELECT state FROM lawpay_refunds`) != "posted" {
		t.Fatal("not posted by the sweep")
	}
	tx, _ := e.lp.Get(r.str(t, "lawpay_refund_id"))
	before := e.moneyState()
	if v := e.hook(fake.RefundWebhook(tx)); v != "applied" || e.moneyState() != before || e.count(`SELECT count(*) FROM exceptions`) != 0 {
		t.Fatalf("webhook %s", v)
	}
}

// MOD-PAY-30: dashboard refunds open external_refund; Post refund records them, a
// partial one as partial.
func TestCoreModPay30(t *testing.T) {
	e := newCoreEnv(t, "CORE-03", "CORE-07", "CORE-23", "CORE-24")
	_, pay, lpID := e.staffCharge("operating", 10000, fake.CardToken)
	_, pay2, lpID2 := e.staffCharge("operating", 8000, fake.CardToken)
	full := e.lp.DashboardRefund(lpID, 10000)
	part := e.lp.DashboardRefund(lpID2, 3000)
	e.hook(fake.RefundWebhook(full))
	e.hook(fake.RefundWebhook(part))
	ex := e.openExceptions("external_refund")
	// Both open at the same instant, so the list order between them is not fixed.
	if len(ex) != 2 || ex[0]["amount_cents"].(float64)+ex[1]["amount_cents"].(float64) != 13000 {
		t.Fatalf("exceptions %+v", ex)
	}
	if e.notes(modID+".exception") == 0 {
		t.Fatal("no notification")
	}
	if r := e.api(http.MethodPost, "/api/exceptions/"+ex[0]["id"].(string)+"/post-refund", coretest.Staff, nil); r.status != http.StatusForbidden {
		t.Fatalf("staff: %d", r.status)
	}
	for _, x := range ex {
		if r := e.form("/tab/post-refund", coretest.Attorney, url.Values{"exception_id": {x["id"].(string)}}); r.status != http.StatusOK {
			t.Fatalf("post: %d %s", r.status, r.body)
		}
	}
	if !e.refundWithRef(pay, full.ID, 10000) || e.payment(pay)["refund_state"] != "full" {
		t.Fatalf("full refund %+v", e.payment(pay))
	}
	if p := e.payment(pay2); p["refund_state"] != "partial" || p["refunded_cents"] != 3000.0 {
		t.Fatalf("partial %+v", p)
	}
	if len(e.openExceptions("external_refund")) != 0 || e.lp.RefundCalls != 0 {
		t.Fatal("still open or refunded again")
	}
}

// noGo records a no-go conflict decision on matter: a contact with the client's name as
// an adverse party elsewhere makes a hit, which the attorney decides no go.
func (e *coreEnv) noGo(matter, clientFamily string) {
	e.t.Helper()
	elsewhere, _ := e.c.Matter("Estate of Vance", "", nil)
	adverse := jsonStr(e.t, e.c.Admin.Must(http.StatusCreated, http.MethodPost, "/api/v1/contacts", map[string]any{"kind": "person",
		"given_name": "Client", "family_name": clientFamily}).JSON, "id")
	e.c.Admin.Must(http.StatusCreated, http.MethodPost, "/api/v1/matter-parties", map[string]any{"matter_id": elsewhere, "contact_id": adverse, "role": "adverse"})
	check := e.c.Attorney.Must(http.StatusCreated, http.MethodPost, "/api/v1/matters/"+matter+"/conflict-checks", map[string]any{}).JSON
	hits, _ := check["hits"].([]any)
	if len(hits) == 0 {
		e.t.Fatalf("no conflict hit to decide: %+v", check)
	}
	hit := hits[0].(map[string]any)["id"].(string)
	e.c.Attorney.Must(http.StatusOK, http.MethodPost, "/api/v1/conflict-checks/"+jsonStr(e.t, check, "id")+"/decisions",
		map[string]any{"hit_id": hit, "decision": "no_go", "reason": "Synthetic conflict"})
	e.deliver()
}

// MOD-PAY-31: a no-go conflict check proposes the consult fee refund once and notifies
// the responsible attorney; Issue refund runs the refund; a matter whose latest payment
// wasn't through LawPay opens manual_refund_needed.
func TestCoreModPay31(t *testing.T) {
	e := newCoreEnv(t, "CORE-03", "CORE-23", "CORE-24", "conflict_check.decided")
	_, pay, _ := e.staffCharge("operating", 17500, fake.CardToken)
	e.noGo(e.matter, "Harlow")
	e.c.DeliverEvents()
	if n := e.count(`SELECT count(*) FROM lawpay_refunds WHERE state = 'proposed' AND amount_cents = 17500 AND payment_id = $1`, pay); n != 1 {
		t.Fatalf("proposed %d", n)
	}
	if got := e.noteUsers(modID + ".refund_proposed"); !got[coretest.Attorney] || len(got) != 1 {
		t.Fatalf("notified %v", got)
	}
	if tab := e.page("/matter", coretest.Attorney, url.Values{"matter_id": {e.matter}}); !strings.Contains(string(tab.body), "Issue refund") {
		t.Fatalf("tab %s", tab.body)
	}
	if r := e.form("/matter/issue", coretest.Attorney, url.Values{"refund_id": {e.text(`SELECT id FROM lawpay_refunds`)}}); r.status != http.StatusOK {
		t.Fatalf("issue %d %s", r.status, r.body)
	}
	if e.refundRows(pay, "completed") != 1 || e.lp.RefundCalls != 1 {
		t.Fatal("not refunded once")
	}
	other, otherContact := e.otherMatter("Mercer")
	e.c.Staff.Must(http.StatusCreated, http.MethodPost, "/api/v1/payments", map[string]any{"contact_id": otherContact, "matter_id": other,
		"amount_cents": 9000, "destination": "operating", "method": "check", "reference_number": "1001"})
	e.noGo(other, "Mercer")
	if ex := e.openExceptions("manual_refund_needed"); len(ex) != 1 || ex[0]["matter_id"] != other {
		t.Fatalf("manual %+v", ex)
	}
}

// MOD-PAY-32: the sweep advances young pending ACH charges and flags old ones once.
func TestCoreModPay32(t *testing.T) {
	e := newCoreEnv(t, "CORE-01", "CORE-02", "CORE-23")
	old, oldPay, _ := e.staffCharge("trust", 20000, fake.BankToken)
	e.clock.Advance(15 * 24 * time.Hour)
	young, youngPay, youngLP := e.staffCharge("trust", 30000, fake.BankToken)
	e.lp.Settle(youngLP, "COMPLETED")
	reads := e.lp.ReadCalls
	if _, err := e.server().Sweep(e.ctx); err != nil {
		t.Fatal(err)
	}
	if e.lp.ReadCalls-reads != 1 {
		t.Fatalf("reads %d", e.lp.ReadCalls-reads)
	}
	if s := e.text(`SELECT state FROM lawpay_charges WHERE id = $1`, young); s != "recorded_completed" || e.payment(youngPay)["status"] != "completed" {
		t.Fatalf("young %s", s)
	}
	if s := e.text(`SELECT state FROM lawpay_charges WHERE id = $1`, old); s != "recorded_pending" || e.payment(oldPay)["status"] != "pending" {
		t.Fatalf("old %s", s)
	}
	if ex := e.openExceptions("pending_ach_stale"); len(ex) != 1 || ex[0]["charge_id"] != old {
		t.Fatalf("exceptions %+v", ex)
	}
	if _, err := e.server().Sweep(e.ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.openExceptions("pending_ach_stale")) != 1 {
		t.Fatal("opened twice")
	}
}

// MOD-PAY-33: the reconciliation tab lists and filters; Resolve needs a note.
func TestCoreModPay33(t *testing.T) {
	e := newCoreEnv(t, "CORE-07", "CORE-23", "CORE-24")
	_, pay, _ := e.staffCharge("operating", 10000, fake.CardToken)
	e.staffCharge("trust", 20000, fake.BankToken)
	e.refund(coretest.Attorney, pay, 1000)
	e.hook(fake.ChargeWebhook(e.lp.AddCharge("acct_operating", "card", 777)))
	all := e.api(http.MethodGet, "/api/transactions", coretest.Staff, nil).json(t)["data"].([]any)
	if len(all) != 3 {
		t.Fatalf("all %d", len(all))
	}
	first := all[0].(map[string]any)
	if first["matter_id"] != e.matter || first["amount_cents"].(float64) != 10000 || first["destination"] != "operating" ||
		first["state"] != "recorded_completed" || first["payment_link"] != "/money/payments/"+pay {
		t.Fatalf("row %+v", first)
	}
	if n := len(e.api(http.MethodGet, "/api/transactions?state=recorded_pending", coretest.Staff, nil).json(t)["data"].([]any)); n != 1 {
		t.Fatalf("pending %d", n)
	}
	if n := len(e.api(http.MethodGet, "/api/transactions?kind=refund", coretest.Staff, nil).json(t)["data"].([]any)); n != 1 {
		t.Fatalf("refunds %d", n)
	}
	tab := e.page("/tab/reconciliation", coretest.Attorney, url.Values{"kind": {"charge"}})
	if tab.status != http.StatusOK || !strings.Contains(string(tab.body), "/money/payments/"+pay) || !strings.Contains(string(tab.body), "Resolve") {
		t.Fatalf("tab %s", tab.body)
	}
	ex := e.openExceptions("")
	if len(ex) != 1 {
		t.Fatalf("exceptions %+v", ex)
	}
	id := ex[0]["id"].(string)
	if r := e.form("/tab/resolve", coretest.Attorney, url.Values{"exception_id": {id}, "note": {"  "}}); r.status != http.StatusUnprocessableEntity {
		t.Fatalf("no note: %d", r.status)
	}
	if r := e.form("/tab/resolve", coretest.Attorney, url.Values{"exception_id": {id}, "note": {"A test charge in the LawPay dashboard."}}); r.status != http.StatusOK {
		t.Fatalf("resolve: %d %s", r.status, r.body)
	}
	if len(e.openExceptions("")) != 0 || e.text(`SELECT resolution_note FROM exceptions`) != "A test charge in the LawPay dashboard." {
		t.Fatal("not resolved")
	}
}
