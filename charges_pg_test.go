package lawpaydocuseal

import (
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/CharitonMedia/togavsu-lawpay-docuseal/fake"
)

// MOD-PAY-01: an empty required setting shows "isn't set up" with the settings link and
// makes no adapter call.
func TestPGModPay01(t *testing.T) {
	e := newEnv(t)
	inv := e.core.AddInvoice(e.matter.ID, 10000)
	e.core.Emit("invoice.sent", "invoice", inv.ID, e.matter.ID, nil)
	e.setting("lawpay_secret_key", "")
	for _, p := range []struct {
		path string
		u    User
		q    url.Values
	}{
		{"/client/pay", e.client, url.Values{"purpose": {"invoice_pay"}, "invoice_id": {inv.ID}}},
		{"/matter/charge", e.staff, url.Values{"matter_id": {e.matter.ID}}},
		{"/matter/refund", e.attorney, url.Values{"payment_id": {"anything"}}},
	} {
		r := e.page(p.path, p.u, p.q)
		if !strings.Contains(html.UnescapeString(string(r.body)), "LawPay isn't set up") || !strings.Contains(string(r.body), settingsLink) {
			t.Fatalf("%s: %d %s", p.path, r.status, r.body)
		}
	}
	if r := e.api(http.MethodPost, "/api/pay-intents", e.staff, map[string]any{"matter_id": e.matter.ID, "amount_cents": 5000}); r.str(t, "code") != "not_configured" {
		t.Fatalf("api: %s", r.body)
	}
	e.setting("lawpay_secret_key", e.secrets["lawpay_secret_key"])
	e.setting("docuseal_api_key", "")
	r := e.page("/matter/send", e.staff, url.Values{"matter_id": {e.matter.ID}})
	if !strings.Contains(html.UnescapeString(string(r.body)), "DocuSeal isn't set up") || !strings.Contains(string(r.body), settingsLink) {
		t.Fatalf("send page: %s", r.body)
	}
	if r := e.api(http.MethodPost, "/api/agreements/x/send", e.staff, nil); r.str(t, "code") != "not_configured" {
		t.Fatalf("send api: %s", r.body)
	}
	if e.lp.Calls() != 0 || e.ds.Calls() != 0 {
		t.Fatalf("adapter calls: lawpay %d docuseal %d", e.lp.Calls(), e.ds.Calls())
	}
}

// MOD-PAY-02: an admin sees secrets redacted; the module's read gets the values; no secret
// reaches a page, log line, receipt or exception.
func TestPGModPay02(t *testing.T) {
	e := newEnv(t)
	admin := e.do(http.MethodGet, e.cored.URL+"/api/v1/modules/lawpay-docuseal/settings",
		map[string]string{"Authorization": "Bearer " + e.core.Token, "X-Togavsu-Context-Token": e.admin.Token}, nil)
	e.noSecrets("admin settings read", string(admin.body))
	if !strings.Contains(string(admin.body), "Redacted") {
		t.Fatalf("admin read: %s", admin.body)
	}
	st, err := e.srv.settings(e.ctx)
	if err != nil || st.LawPaySecret != e.secrets["lawpay_secret_key"] || st.DocuSealKey != e.secrets["docuseal_api_key"] {
		t.Fatalf("module read: %v", err)
	}
	e.noSecrets("Settings.String", st.String()+st.GoString())
	// Exercise pages, a webhook receipt, an exception and the logs.
	e.lawpayHook(e.secrets["lawpay_webhook_token"], fake.ChargeWebhook(e.lp.AddCharge("acct_operating", "card", 1234)))
	e.lawpayHook(e.secrets["lawpay_webhook_token"], []byte(`{"token":"`+e.secrets["lawpay_webhook_token"]+`"`))
	e.core.FailNext("createPayment", 1)
	e.pay(e.staff, map[string]any{"matter_id": e.matter.ID, "amount_cents": 2500}, fake.CardToken)
	for _, p := range []string{"/tab/setup", "/tab/reconciliation", "/matter/charge", "/matter"} {
		r := e.page(p, e.admin, url.Values{"matter_id": {e.matter.ID}})
		if r.status != http.StatusOK {
			t.Fatalf("%s: %d %s", p, r.status, r.body)
		}
		e.noSecrets(p, string(r.body))
	}
	if r := e.form("/tab/setup", e.admin, url.Values{}); r.status != http.StatusOK || !strings.Contains(string(r.body), "LawPay: ok") {
		t.Fatalf("test connection: %d %s", r.status, r.body)
	}
	e.noSecrets("receipts", e.text(`SELECT string_agg(payload::text, ' ') FROM lawpay_webhook_receipts WHERE verdict <> 'malformed'`))
	e.noSecrets("exceptions", e.text(`SELECT string_agg(detail, ' ') FROM exceptions`))
	e.noSecrets("logs", e.logs.String())
}

// MOD-PAY-03: an inactive core trust account blocks trust charges and opens
// account_mismatch with a notification; operating charges keep working.
func TestPGModPay03(t *testing.T) {
	e := newEnv(t)
	inactive := e.core.AddTrustAccount(false)
	e.setting("core_trust_account_id", inactive)
	if n := e.count(`SELECT count(*) FROM exceptions WHERE kind = 'account_mismatch' AND status = 'open'`); n != 1 {
		t.Fatalf("exceptions %d", n)
	}
	notes, _, _ := e.core.Snapshot()
	if len(notes) != 1 || notes[0].Kind != "lawpay-docuseal.exception" {
		t.Fatalf("notifications %+v", notes)
	}
	r := e.api(http.MethodPost, "/api/pay-intents", e.staff, map[string]any{"matter_id": e.matter.ID, "amount_cents": 5000, "destination": "trust"})
	if r.status != http.StatusConflict || r.str(t, "code") != "not_configured" {
		t.Fatalf("trust: %d %s", r.status, r.body)
	}
	if e.lp.ChargeCalls != 0 {
		t.Fatal("charged")
	}
	if _, pay, _ := e.staffCharge("operating", 5000, fake.CardToken); pay == "" {
		t.Fatal("operating charge failed")
	}
	// A second refresh doesn't notify twice.
	e.srv.RefreshState(e.ctx)
	if notes, _, _ := e.core.Snapshot(); len(notes) != 1 {
		t.Fatalf("notified again: %d", len(notes))
	}
}

// MOD-PAY-04: staff Take payment of 25000 cents to operating.
func TestPGModPay04(t *testing.T) {
	e := newEnv(t)
	charge, pay, lpID := e.staffCharge("operating", 25000, fake.CardToken)
	if n := e.core.CallCount("initiateProcessorPayment"); n != 1 {
		t.Fatalf("initializations %d", n)
	}
	if e.lp.ChargeCalls != 1 || e.lp.Charges[0].AccountID != "acct_operating" || e.lp.Charges[0].AmountCents != 25000 || e.lp.Charges[0].Reference != charge {
		t.Fatalf("adapter %+v", e.lp.Charges)
	}
	ps := e.core.Payments()
	if len(ps) != 1 {
		t.Fatalf("payments %d", len(ps))
	}
	p := ps[0]
	if p.ID != pay || p.Method != "card" || p.Destination != "operating" || p.Reference != lpID || p.Source != "lawpay-docuseal" || p.Status != "completed" {
		t.Fatalf("payment %+v", p)
	}
	if s := e.text(`SELECT state FROM lawpay_charges WHERE id = $1`, charge); s != "recorded_completed" {
		t.Fatalf("state %s", s)
	}
	if calls := e.core.CallsOf("createPayment"); len(calls) != 1 || calls[0].Kind != "task" || calls[0].Idempotency != "lpds-charge-"+charge {
		t.Fatalf("createPayment calls %+v", calls)
	}
}

// MOD-PAY-05: the same to trust.
func TestPGModPay05(t *testing.T) {
	e := newEnv(t)
	_, pay, _ := e.staffCharge("trust", 25000, fake.CardToken)
	if e.lp.Charges[0].AccountID != "acct_trust" {
		t.Fatalf("account %s", e.lp.Charges[0].AccountID)
	}
	p := e.core.PaymentRecord(pay)
	if p.Destination != "trust" || p.TrustAccountID != e.trustAccount {
		t.Fatalf("payment %+v", p)
	}
	if rows := e.core.TrustEntries("deposit"); len(rows) != 1 || rows[0].Cents != 25000 {
		t.Fatalf("trust rows %+v", rows)
	}
}

// MOD-PAY-06: ACH authorized records pending with no trust row; a verified COMPLETED
// webhook completes it once.
func TestPGModPay06(t *testing.T) {
	e := newEnv(t)
	charge, pay, lpID := e.staffCharge("trust", 40000, fake.BankToken)
	if p := e.core.PaymentRecord(pay); p.Status != "pending" || p.Method != "ach" {
		t.Fatalf("payment %+v", p)
	}
	if rows := e.core.TrustEntries("deposit"); len(rows) != 0 {
		t.Fatalf("trust rows before settlement %d", len(rows))
	}
	e.lp.Settle(lpID, "COMPLETED")
	tx, _ := e.lp.Get(lpID)
	if r := e.lawpayHook(e.secrets["lawpay_webhook_token"], fake.ChargeWebhook(tx)); r.status != http.StatusOK {
		t.Fatalf("hook %d %s", r.status, r.body)
	}
	if p := e.core.PaymentRecord(pay); p.Status != "completed" {
		t.Fatalf("payment %+v", p)
	}
	if rows := e.core.TrustEntries("deposit"); len(rows) != 1 {
		t.Fatalf("trust rows %d", len(rows))
	}
	// A second COMPLETED delivery (a new body) posts nothing.
	body := strings.Replace(string(fake.ChargeWebhook(tx)), `"event"`, `"retry":1,"event"`, 1)
	e.lawpayHook(e.secrets["lawpay_webhook_token"], []byte(body))
	if rows := e.core.TrustEntries("deposit"); len(rows) != 1 {
		t.Fatalf("trust rows after second webhook %d", len(rows))
	}
	if s := e.text(`SELECT state FROM lawpay_charges WHERE id = $1`, charge); s != "recorded_completed" {
		t.Fatalf("state %s", s)
	}
}

// MOD-PAY-07: amounts outside the limits are refused before any core or adapter call.
func TestPGModPay07(t *testing.T) {
	e := newEnv(t)
	before := e.core.CallCount("")
	for _, cents := range []int64{99, 5000001} {
		r := e.api(http.MethodPost, "/api/pay-intents", e.staff, map[string]any{"matter_id": e.matter.ID, "amount_cents": cents})
		if r.status != http.StatusUnprocessableEntity || r.str(t, "code") != "amount_out_of_range" {
			t.Fatalf("%d: %d %s", cents, r.status, r.body)
		}
	}
	// Only the context-token verify and the settings read happen per request.
	for _, c := range e.core.Calls[before:] {
		if c.Op != "verifyContextToken" && c.Op != "getModuleSettings" {
			t.Fatalf("core call %s", c.Op)
		}
	}
	if e.lp.Calls() != 0 || e.count(`SELECT count(*) FROM lawpay_charges`) != 0 {
		t.Fatal("charged or stored")
	}
}

// MOD-PAY-08: a declined card leaves no core payment, a declined row with a PAY-CHRG code
// and shows "Card declined".
func TestPGModPay08(t *testing.T) {
	e := newEnv(t)
	e.lp.Script("decline")
	in := e.form("/matter/charge", e.staff, url.Values{"matter_id": {e.matter.ID}, "amount": {"150.00"}, "destination": {"operating"}})
	if in.status != http.StatusOK || !strings.Contains(string(in.body), "pay-form") {
		t.Fatalf("intent page %d %s", in.status, in.body)
	}
	id := e.text(`SELECT id FROM lawpay_charges`)
	r := e.form("/matter/submit", e.staff, url.Values{"intent_id": {id}, "token": {fake.CardToken}})
	if !strings.Contains(string(r.body), "Card declined") {
		t.Fatalf("result %s", r.body)
	}
	if len(e.core.Payments()) != 0 {
		t.Fatal("payment recorded")
	}
	if s, code := e.text(`SELECT state FROM lawpay_charges`), e.text(`SELECT failure_code FROM lawpay_charges`); s != "declined" || !strings.HasPrefix(code, "PAY-CHRG") {
		t.Fatalf("row %s %s", s, code)
	}
}

// MOD-PAY-09: a double submit charges once and returns the first result; an expired intent
// charges nothing.
func TestPGModPay09(t *testing.T) {
	e := newEnv(t)
	in := e.api(http.MethodPost, "/api/pay-intents", e.staff, map[string]any{"matter_id": e.matter.ID, "amount_cents": 7000})
	id := in.str(t, "id")
	type out struct {
		r resp
	}
	results := make(chan resp, 2)
	for range 2 {
		go func() {
			results <- e.api(http.MethodPost, "/api/pay-intents/"+id+"/submit", e.staff, map[string]string{"token": fake.CardToken})
		}()
	}
	a, b := <-results, <-results
	if a.status != http.StatusOK || b.status != http.StatusOK || a.str(t, "payment_id") == "" || a.str(t, "payment_id") != b.str(t, "payment_id") {
		t.Fatalf("results %s / %s", a.body, b.body)
	}
	third := e.api(http.MethodPost, "/api/pay-intents/"+id+"/submit", e.staff, map[string]string{"token": fake.CardToken})
	if third.str(t, "payment_id") != a.str(t, "payment_id") || e.lp.ChargeCalls != 1 || len(e.core.Payments()) != 1 {
		t.Fatalf("charged %d times", e.lp.ChargeCalls)
	}
	old := e.api(http.MethodPost, "/api/pay-intents", e.staff, map[string]any{"matter_id": e.matter.ID, "amount_cents": 7000})
	e.clock.Advance(31 * time.Minute)
	r := e.api(http.MethodPost, "/api/pay-intents/"+old.str(t, "id")+"/submit", e.staff, map[string]string{"token": fake.CardToken})
	if r.str(t, "state") != "expired" || e.lp.ChargeCalls != 1 {
		t.Fatalf("expired: %s, charges %d", r.body, e.lp.ChargeCalls)
	}
}

// MOD-PAY-10: a client's fourth card attempt inside the window is refused with 429.
func TestPGModPay10(t *testing.T) {
	e := newEnv(t)
	e.srv.depositsOn(e.ctx, e.matter.ID, "")
	for i := range 3 {
		e.lp.Script("decline")
		_, r := e.pay(e.client, map[string]any{"purpose": "trust_deposit", "matter_id": e.matter.ID, "amount_cents": 5000}, fake.CardToken)
		if r.str(t, "state") != "declined" {
			t.Fatalf("attempt %d: %s", i+1, r.body)
		}
	}
	_, r := e.pay(e.client, map[string]any{"purpose": "trust_deposit", "matter_id": e.matter.ID, "amount_cents": 5000}, fake.CardToken)
	if r.status != http.StatusTooManyRequests || e.lp.ChargeCalls != 3 {
		t.Fatalf("fourth: %d %s, charges %d", r.status, r.body, e.lp.ChargeCalls)
	}
	e.clock.Advance(11 * time.Minute)
	if _, r := e.pay(e.client, map[string]any{"purpose": "trust_deposit", "matter_id": e.matter.ID, "amount_cents": 5000}, fake.CardToken); r.str(t, "state") != "recorded_completed" {
		t.Fatalf("after the window: %s", r.body)
	}
}

// MOD-PAY-11: a replenishment deposit charges exactly the request with no amount field and
// satisfies it; without it, an amount under the minimum is refused before any call.
func TestPGModPay11(t *testing.T) {
	e := newEnv(t)
	rr := e.core.AddReplenishment(e.matter.ID, e.clientContact.ID, 50000)
	e.core.Emit("trust.replenishment_requested", "trust_replenishment_request", rr.ID, e.matter.ID, nil)
	page := e.page("/client/pay", e.client, url.Values{"purpose": {"trust_deposit"}, "matter_id": {e.matter.ID}, "replenishment_request_id": {rr.ID}})
	if page.status != http.StatusOK || strings.Contains(string(page.body), `name="amount"`) || !strings.Contains(string(page.body), "$500.00") {
		t.Fatalf("page %d %s", page.status, page.body)
	}
	id := e.text(`SELECT id FROM lawpay_charges`)
	r := e.form("/client/submit", e.client, url.Values{"intent_id": {id}, "token": {fake.CardToken}})
	if !strings.Contains(string(r.body), "Payment received") {
		t.Fatalf("result %s", r.body)
	}
	if e.lp.Charges[0].AmountCents != 50000 {
		t.Fatalf("charged %d", e.lp.Charges[0].AmountCents)
	}
	if got := e.core.ReplenishmentRecord(rr.ID); got.Status != "satisfied" {
		t.Fatalf("request %s", got.Status)
	}
	if init := e.core.CallsOf("initiateProcessorPayment"); len(init) != 1 || init[0].Kind != "viewer" {
		t.Fatalf("init %+v", init)
	}
	// Open deposit: the amount field shows, and an amount under the minimum is refused.
	open := e.page("/client/pay", e.client, url.Values{"purpose": {"trust_deposit"}, "matter_id": {e.matter.ID}})
	if !strings.Contains(string(open.body), `name="amount"`) {
		t.Fatalf("open deposit page %s", open.body)
	}
	writes, calls, reads := e.core.Writes(), e.lp.Calls(), e.core.CallCount("getMatter")
	low := e.form("/client/pay", e.client, url.Values{"purpose": {"trust_deposit"}, "matter_id": {e.matter.ID}, "amount": {"0.50"}})
	if low.status != http.StatusUnprocessableEntity || e.core.Writes() != writes || e.lp.Calls() != calls || e.core.CallCount("getMatter") != reads {
		t.Fatalf("low amount: %d %s", low.status, low.body)
	}
}

// MOD-PAY-12: invoice payment to trust records a trust deposit naming no invoice; to
// operating it names the invoice and lowers the balance.
func TestPGModPay12(t *testing.T) {
	e := newEnv(t)
	inv := e.core.AddInvoice(e.matter.ID, 30000)
	e.core.Emit("invoice.sent", "invoice", inv.ID, e.matter.ID, nil)
	_, r := e.pay(e.client, map[string]any{"purpose": "invoice_pay", "invoice_id": inv.ID}, fake.CardToken)
	if r.str(t, "state") != "recorded_completed" || e.lp.Charges[0].AccountID != "acct_trust" {
		t.Fatalf("trust pay: %s", r.body)
	}
	p := e.core.PaymentRecord(r.str(t, "payment_id"))
	if p.Destination != "trust" || p.InvoiceID != "" || e.core.InvoiceRecord(inv.ID).PaidCents != 0 {
		t.Fatalf("payment %+v", p)
	}
	if got := e.text(`SELECT invoice_id FROM lawpay_charges`); got != inv.ID {
		t.Fatalf("charge names %q", got)
	}
	e.setting("invoice_pay_destination", "operating")
	inv2 := e.core.AddInvoice(e.matter.ID, 12000)
	e.core.Emit("invoice.sent", "invoice", inv2.ID, e.matter.ID, nil)
	_, r = e.pay(e.client, map[string]any{"purpose": "invoice_pay", "invoice_id": inv2.ID}, fake.CardToken)
	p = e.core.PaymentRecord(r.str(t, "payment_id"))
	if p.Destination != "operating" || p.InvoiceID != inv2.ID || e.core.InvoiceRecord(inv2.ID).PaidCents != 12000 {
		t.Fatalf("operating payment %+v", p)
	}
}

// MOD-PAY-13: a client can't pay another matter's deposit or another client's invoice, or
// use another client's replenishment request; each answers 404 with no write or charge.
func TestPGModPay13(t *testing.T) {
	e := newEnv(t)
	other, otherMatter := e.newClient("Casey")
	_ = other
	e.srv.depositsOn(e.ctx, otherMatter.ID, "")
	inv := e.core.AddInvoice(otherMatter.ID, 9000)
	e.core.Emit("invoice.sent", "invoice", inv.ID, otherMatter.ID, nil)
	rrOther := e.core.AddReplenishment(otherMatter.ID, other.ContactID, 20000)
	e.srv.depositsOn(e.ctx, e.matter.ID, "")
	rrWrongMatter := e.core.AddReplenishment(otherMatter.ID, e.clientContact.ID, 20000)
	writes := e.core.Writes()
	for _, q := range []url.Values{
		{"purpose": {"trust_deposit"}, "matter_id": {otherMatter.ID}, "amount": {"100.00"}},
		{"purpose": {"invoice_pay"}, "invoice_id": {inv.ID}},
		{"purpose": {"trust_deposit"}, "matter_id": {e.matter.ID}, "replenishment_request_id": {rrOther.ID}},
		{"purpose": {"trust_deposit"}, "matter_id": {e.matter.ID}, "replenishment_request_id": {rrWrongMatter.ID}},
	} {
		r := e.form("/client/pay", e.client, q)
		if r.status != http.StatusNotFound {
			t.Fatalf("%v: %d %s", q, r.status, r.body)
		}
	}
	if e.core.Writes() != writes || e.lp.Calls() != 0 || e.count(`SELECT count(*) FROM lawpay_charges`) != 0 {
		t.Fatal("something was written or charged")
	}
}

// MOD-PAY-14: createPayment fails after the charge: the payer sees the LawPay id, the row
// is charged_unrecorded, the sweep retries with the same key, and after retry_limit one
// exception and one notification.
func TestPGModPay14(t *testing.T) {
	e := newEnv(t)
	e.core.FailNext("createPayment", 4)
	_, r := e.pay(e.staff, map[string]any{"matter_id": e.matter.ID, "amount_cents": 8800}, fake.CardToken)
	lp := r.str(t, "lawpay_charge_id")
	if r.str(t, "state") != "charged_unrecorded" || !strings.Contains(r.str(t, "message"), lp) || !strings.Contains(r.str(t, "message"), "contact the firm") {
		t.Fatalf("result %s", r.body)
	}
	for i := range 3 {
		e.srv.Sweep(e.ctx)
		if n := e.count(`SELECT count(*) FROM exceptions WHERE kind = 'charged_unrecorded'`); (i < 2 && n != 0) || (i == 2 && n != 1) {
			t.Fatalf("sweep %d: exceptions %d", i+1, n)
		}
	}
	keys := map[string]bool{}
	for _, c := range e.core.CallsOf("createPayment") {
		keys[c.Idempotency] = true
	}
	if len(e.core.CallsOf("createPayment")) != 4 || len(keys) != 1 {
		t.Fatalf("createPayment calls %+v", e.core.CallsOf("createPayment"))
	}
	notes, _, _ := e.core.Snapshot()
	if len(notes) != 1 || notes[0].Audience != "admins_and_responsible_attorney" {
		t.Fatalf("notifications %+v", notes)
	}
	e.srv.Sweep(e.ctx)
	if p := e.core.Payments(); len(p) != 1 || p[0].Reference != lp {
		t.Fatalf("payments after recovery %+v", p)
	}
	if notes, _, _ := e.core.Snapshot(); len(notes) != 1 || e.lp.ChargeCalls != 1 {
		t.Fatal("notified or charged again")
	}
}
