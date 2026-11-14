package acceptance

import (
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/CharitonMedia/togavsu/pkg/modulekit/coretest"

	"github.com/CharitonMedia/togavsu-lawpay-docuseal/fake"
)

const settingsLink = "/admin?tab=modules"

// MOD-PAY-01: an empty required setting shows "isn't set up" with the settings link and
// makes no adapter call.
func TestCoreModPay01(t *testing.T) {
	e := newCoreEnv(t)
	inv, _ := e.invoice(e.matter, 5)
	e.setting("lawpay_secret_key", "")
	for _, p := range []struct {
		path, who string
		q         url.Values
	}{
		{"/client/pay", coretest.ClientRole, url.Values{"purpose": {"invoice_pay"}, "invoice_id": {inv}}},
		{"/matter/charge", coretest.Staff, url.Values{"matter_id": {e.matter}}},
		{"/matter/refund", coretest.Attorney, url.Values{"payment_id": {"anything"}}},
	} {
		r := e.page(p.path, p.who, p.q)
		if !strings.Contains(html.UnescapeString(string(r.body)), "LawPay isn't set up") || !strings.Contains(string(r.body), settingsLink) {
			t.Fatalf("%s: %d %s", p.path, r.status, r.body)
		}
	}
	if r := e.api(http.MethodPost, "/api/pay-intents", coretest.Staff, map[string]any{"matter_id": e.matter, "amount_cents": 5000}); r.str(t, "code") != "not_configured" {
		t.Fatalf("api: %s", r.body)
	}
	e.setting("lawpay_secret_key", e.secrets["lawpay_secret_key"])
	e.setting("docuseal_api_key", "")
	r := e.page("/matter/send", coretest.Staff, url.Values{"matter_id": {e.matter}})
	if !strings.Contains(html.UnescapeString(string(r.body)), "DocuSeal isn't set up") || !strings.Contains(string(r.body), settingsLink) {
		t.Fatalf("send page: %s", r.body)
	}
	if r := e.api(http.MethodPost, "/api/agreements/x/send", coretest.Staff, nil); r.str(t, "code") != "not_configured" {
		t.Fatalf("send api: %s", r.body)
	}
	if e.lp.Calls() != 0 || e.ds.Calls() != 0 {
		t.Fatalf("adapter calls: lawpay %d docuseal %d", e.lp.Calls(), e.ds.Calls())
	}
}

// MOD-PAY-02: an admin sees every secret redacted; the module's own read gets the
// values; no secret reaches a page, log line, receipt or exception.
func TestCoreModPay02(t *testing.T) {
	e := newCoreEnv(t, "CORE-07", "CORE-23")
	admin := e.c.Admin.Must(http.StatusOK, http.MethodGet, "/api/v1/modules/"+modID+"/settings", nil)
	e.noSecrets("admin settings read", admin.Body)
	own := e.c.ModuleClient(modID).Do(http.MethodGet, "/api/v1/modules/"+modID+"/settings", nil)
	for k, v := range e.secrets {
		if !strings.Contains(own.Body, v) {
			t.Fatalf("the module's own read lacks %s: %d", k, own.Code)
		}
	}
	e.lawpayHook(e.secrets["lawpay_webhook_token"], fake.ChargeWebhook(e.lp.AddCharge("acct_operating", "card", 1234)))
	e.lawpayHook(e.secrets["lawpay_webhook_token"], []byte(`{"token":"`+e.secrets["lawpay_webhook_token"]+`"`))
	e.tr.failNext(http.MethodPost, "/api/v1/payments", 1)
	e.pay(coretest.Staff, map[string]any{"matter_id": e.matter, "amount_cents": 2500}, fake.CardToken)
	for _, p := range []string{"/tab/setup", "/tab/reconciliation", "/matter/charge", "/matter"} {
		r := e.page(p, coretest.Admin, url.Values{"matter_id": {e.matter}})
		if r.status != http.StatusOK {
			t.Fatalf("%s: %d %s", p, r.status, r.body)
		}
		e.noSecrets(p, string(r.body))
	}
	if r := e.form("/tab/setup", coretest.Admin, url.Values{}); r.status != http.StatusOK || !strings.Contains(string(r.body), "LawPay: ok") {
		t.Fatalf("test connection: %d %s", r.status, r.body)
	}
	e.noSecrets("receipts", e.text(`SELECT string_agg(payload::text, ' ') FROM lawpay_webhook_receipts WHERE verdict <> 'malformed'`))
	e.noSecrets("exceptions", e.text(`SELECT string_agg(detail, ' ') FROM exceptions`))
	e.noSecrets("logs", e.logs.String())
}

// MOD-PAY-03: an inactive core trust account blocks trust charges and opens
// account_mismatch with a notification; operating charges keep working.
func TestCoreModPay03(t *testing.T) {
	e := newCoreEnv(t, "CORE-03", "CORE-23")
	e.setting("core_trust_account_id", e.newTrustAccount(false))
	if n := e.count(`SELECT count(*) FROM exceptions WHERE kind = 'account_mismatch' AND status = 'open'`); n != 1 {
		t.Fatalf("exceptions %d", n)
	}
	if n := e.notes(modID + ".exception"); n == 0 {
		t.Fatal("no exception notification")
	}
	sent := e.notes(modID + ".exception")
	r := e.api(http.MethodPost, "/api/pay-intents", coretest.Staff, map[string]any{"matter_id": e.matter, "amount_cents": 5000, "destination": "trust"})
	if r.status != http.StatusConflict || r.str(t, "code") != "not_configured" || e.lp.ChargeCalls != 0 {
		t.Fatalf("trust: %d %s", r.status, r.body)
	}
	if _, pay, _ := e.staffCharge("operating", 5000, fake.CardToken); pay == "" {
		t.Fatal("operating charge failed")
	}
	if _, err := e.server().RefreshState(e.ctx); err != nil {
		t.Fatal(err)
	}
	if n := e.notes(modID + ".exception"); n != sent {
		t.Fatalf("notified again: %d, then %d", sent, n)
	}
}

// MOD-PAY-04: staff Take payment of 25000 cents to operating.
func TestCoreModPay04(t *testing.T) {
	e := newCoreEnv(t, "CORE-23")
	charge, pay, lpID := e.staffCharge("operating", 25000, fake.CardToken)
	if n := len(e.tr.of(http.MethodPost, "/api/v1/payments/processor-initializations")); n != 1 {
		t.Fatalf("initializations %d", n)
	}
	if e.lp.ChargeCalls != 1 || e.lp.Charges[0].AccountID != "acct_operating" || e.lp.Charges[0].AmountCents != 25000 {
		t.Fatalf("adapter %+v", e.lp.Charges)
	}
	ps := e.payments()
	if len(ps) != 1 {
		t.Fatalf("payments %d", len(ps))
	}
	p := ps[0]
	if p["id"] != pay || p["method"] != "card" || p["destination"] != "operating" || p["reference_number"] != lpID ||
		p["source"] != modID || p["status"] != "completed" || p["amount_cents"] != 25000.0 {
		t.Fatalf("payment %+v", p)
	}
	if s := e.text(`SELECT state FROM lawpay_charges WHERE id = $1`, charge); s != "recorded_completed" {
		t.Fatalf("state %s", s)
	}
	calls := e.tr.of(http.MethodPost, "/api/v1/payments")
	var creates []coreCall
	for _, c := range calls {
		if c.Path == "/api/v1/payments" {
			creates = append(creates, c)
		}
	}
	if len(creates) != 1 || creates[0].Header.Get("X-Togavsu-Task-Authorization") == "" || creates[0].Header.Get("Idempotency-Key") != "lpds-charge-"+charge {
		t.Fatalf("createPayment calls %+v", creates)
	}
}

// MOD-PAY-05: the same to trust: the trust account is charged, the core payment is a
// trust deposit on core_trust_account_id, and the core posts one trust row.
func TestCoreModPay05(t *testing.T) {
	e := newCoreEnv(t, "CORE-23")
	_, pay, _ := e.staffCharge("trust", 25000, fake.CardToken)
	if e.lp.Charges[0].AccountID != "acct_trust" {
		t.Fatalf("account %s", e.lp.Charges[0].AccountID)
	}
	if p := e.payment(pay); p["destination"] != "trust" || p["trust_account_id"] != e.trustAccount {
		t.Fatalf("payment %+v", p)
	}
	if n := e.c.Count(`SELECT count(*) FROM core.trust_ledger WHERE entry_type = 'deposit' AND amount_cents = 25000 AND trust_account_id = $1`, e.trustAccount); n != 1 {
		t.Fatalf("trust rows %d", n)
	}
}

// MOD-PAY-06: ACH authorized records pending with no trust row; a verified COMPLETED
// webhook completes it once.
func TestCoreModPay06(t *testing.T) {
	e := newCoreEnv(t, "CORE-01", "CORE-02", "CORE-07", "CORE-23")
	charge, pay, lpID := e.staffCharge("trust", 40000, fake.BankToken)
	if p := e.payment(pay); p["status"] != "pending" || p["method"] != "ach" {
		t.Fatalf("payment %+v", p)
	}
	if n := e.trustRows("deposit"); n != 0 {
		t.Fatalf("trust rows before settlement %d", n)
	}
	e.lp.Settle(lpID, "COMPLETED")
	tx, _ := e.lp.Get(lpID)
	if v := e.hook(fake.ChargeWebhook(tx)); v != "applied" {
		t.Fatalf("verdict %s", v)
	}
	if p := e.payment(pay); p["status"] != "completed" || e.trustRows("deposit") != 1 {
		t.Fatalf("payment %+v, trust rows %d", p, e.trustRows("deposit"))
	}
	body := strings.Replace(string(fake.ChargeWebhook(tx)), `"event"`, `"retry":1,"event"`, 1)
	e.hook([]byte(body))
	if e.trustRows("deposit") != 1 || e.text(`SELECT state FROM lawpay_charges WHERE id = $1`, charge) != "recorded_completed" {
		t.Fatal("a second COMPLETED webhook posted again")
	}
}

// MOD-PAY-07: amounts outside the limits are refused before any core or adapter call.
func TestCoreModPay07(t *testing.T) {
	e := newCoreEnv(t)
	writes := e.tr.writes()
	for _, cents := range []int64{99, 5000001} {
		r := e.api(http.MethodPost, "/api/pay-intents", coretest.Staff, map[string]any{"matter_id": e.matter, "amount_cents": cents})
		if r.status != http.StatusUnprocessableEntity || r.str(t, "code") != "amount_out_of_range" {
			t.Fatalf("%d: %d %s", cents, r.status, r.body)
		}
	}
	if e.tr.writes() != writes || len(e.tr.of(http.MethodGet, "/api/v1/matters")) != 0 || e.lp.Calls() != 0 || e.count(`SELECT count(*) FROM lawpay_charges`) != 0 {
		t.Fatal("called the core or LawPay, or stored a charge")
	}
}

// MOD-PAY-08: a declined card leaves no core payment, a declined row with a PAY-CHRG
// code, and shows "Card declined".
func TestCoreModPay08(t *testing.T) {
	e := newCoreEnv(t, "CORE-23")
	e.lp.Script("decline")
	in := e.form("/matter/charge", coretest.Staff, url.Values{"matter_id": {e.matter}, "amount": {"150.00"}, "destination": {"operating"}})
	if in.status != http.StatusOK || !strings.Contains(string(in.body), "pay-form") {
		t.Fatalf("intent page %d %s", in.status, in.body)
	}
	r := e.form("/matter/submit", coretest.Staff, url.Values{"intent_id": {e.text(`SELECT id FROM lawpay_charges`)}, "token": {fake.CardToken}})
	if !strings.Contains(string(r.body), "Card declined") || len(e.payments()) != 0 {
		t.Fatalf("result %s", r.body)
	}
	if s, code := e.text(`SELECT state FROM lawpay_charges`), e.text(`SELECT failure_code FROM lawpay_charges`); s != "declined" || !strings.HasPrefix(code, "PAY-CHRG") {
		t.Fatalf("row %s %s", s, code)
	}
}

// MOD-PAY-09: a double submit charges once and returns the first result; an expired
// intent charges nothing.
func TestCoreModPay09(t *testing.T) {
	e := newCoreEnv(t, "CORE-23")
	in := e.api(http.MethodPost, "/api/pay-intents", coretest.Staff, map[string]any{"matter_id": e.matter, "amount_cents": 7000})
	id := in.str(t, "id")
	results := make(chan resp, 2)
	for i := 0; i < 2; i++ {
		go func() {
			results <- e.api(http.MethodPost, "/api/pay-intents/"+id+"/submit", coretest.Staff, map[string]string{"token": fake.CardToken})
		}()
	}
	a, b := <-results, <-results
	if a.status != http.StatusOK || b.status != http.StatusOK || a.str(t, "payment_id") == "" || a.str(t, "payment_id") != b.str(t, "payment_id") {
		t.Fatalf("results %s / %s", a.body, b.body)
	}
	if e.lp.ChargeCalls != 1 || len(e.payments()) != 1 {
		t.Fatalf("charged %d times", e.lp.ChargeCalls)
	}
	old := e.api(http.MethodPost, "/api/pay-intents", coretest.Staff, map[string]any{"matter_id": e.matter, "amount_cents": 7000})
	e.clock.Advance(31 * time.Minute)
	r := e.api(http.MethodPost, "/api/pay-intents/"+old.str(t, "id")+"/submit", coretest.Staff, map[string]string{"token": fake.CardToken})
	if r.str(t, "state") != "expired" || e.lp.ChargeCalls != 1 {
		t.Fatalf("expired: %s, charges %d", r.body, e.lp.ChargeCalls)
	}
}

// MOD-PAY-10: a client's fourth card attempt inside the window is refused with 429.
func TestCoreModPay10(t *testing.T) {
	e := newCoreEnv(t, "CORE-10", "CORE-23")
	e.api(http.MethodPost, "/api/matters/"+e.matter+"/deposits", coretest.Staff, map[string]bool{"enabled": true})
	deposit := map[string]any{"purpose": "trust_deposit", "matter_id": e.matter, "amount_cents": 5000}
	for i := 0; i < 3; i++ {
		e.lp.Script("decline")
		if _, r := e.pay(coretest.ClientRole, deposit, fake.CardToken); r.str(t, "state") != "declined" {
			t.Fatalf("attempt %d: %s", i+1, r.body)
		}
	}
	if _, r := e.pay(coretest.ClientRole, deposit, fake.CardToken); r.status != http.StatusTooManyRequests || e.lp.ChargeCalls != 3 {
		t.Fatalf("fourth: %d %s, charges %d", r.status, r.body, e.lp.ChargeCalls)
	}
	e.clock.Advance(11 * time.Minute)
	if _, r := e.pay(coretest.ClientRole, deposit, fake.CardToken); r.str(t, "state") != "recorded_completed" {
		t.Fatalf("after the window: %s", r.body)
	}
}

// MOD-PAY-11: a replenishment deposit charges exactly the request with no amount field
// and satisfies it (C5); without it, an amount under the minimum is refused before any
// call.
func TestCoreModPay11(t *testing.T) {
	e := newCoreEnv(t, "CORE-10", "CORE-12", "CORE-23")
	rr := e.replenish(e.matter, e.client, 50000)
	page := e.page("/client/pay", coretest.ClientRole, url.Values{"purpose": {"trust_deposit"}, "matter_id": {e.matter}, "replenishment_request_id": {rr}})
	if page.status != http.StatusOK || strings.Contains(string(page.body), `name="amount"`) || !strings.Contains(string(page.body), "$500.00") {
		t.Fatalf("page %d %s", page.status, page.body)
	}
	r := e.form("/client/submit", coretest.ClientRole, url.Values{"intent_id": {e.text(`SELECT id FROM lawpay_charges`)}, "token": {fake.CardToken}})
	if !strings.Contains(string(r.body), "Payment received") || e.lp.Charges[0].AmountCents != 50000 {
		t.Fatalf("result %s", r.body)
	}
	if got := e.c.Admin.Must(http.StatusOK, http.MethodGet, "/api/v1/trust-replenishment-requests/"+rr, nil).JSON; got["status"] != "satisfied" {
		t.Fatalf("request %+v", got)
	}
	if init := e.tr.of(http.MethodPost, "/api/v1/payments/processor-initializations"); len(init) != 1 || init[0].Header.Get(coretest.ContextHeader) == "" {
		t.Fatalf("initialization not a viewer call: %+v", init)
	}
	open := e.page("/client/pay", coretest.ClientRole, url.Values{"purpose": {"trust_deposit"}, "matter_id": {e.matter}})
	if !strings.Contains(string(open.body), `name="amount"`) {
		t.Fatalf("open deposit page %s", open.body)
	}
	writes, calls := e.tr.writes(), e.lp.Calls()
	low := e.form("/client/pay", coretest.ClientRole, url.Values{"purpose": {"trust_deposit"}, "matter_id": {e.matter}, "amount": {"0.50"}})
	if low.status != http.StatusUnprocessableEntity || e.tr.writes() != writes || e.lp.Calls() != calls {
		t.Fatalf("low amount: %d %s", low.status, low.body)
	}
}

// MOD-PAY-12: invoice payment to trust records a trust deposit naming no invoice; to
// operating it names the invoice and lowers the balance.
func TestCoreModPay12(t *testing.T) {
	e := newCoreEnv(t, "CORE-10", "CORE-23")
	inv, total := e.invoice(e.matter, 5)
	_, r := e.pay(coretest.ClientRole, map[string]any{"purpose": "invoice_pay", "invoice_id": inv}, fake.CardToken)
	if r.str(t, "state") != "recorded_completed" || e.lp.Charges[0].AccountID != "acct_trust" || e.lp.Charges[0].AmountCents != total {
		t.Fatalf("trust pay: %s", r.body)
	}
	if p := e.payment(r.str(t, "payment_id")); p["destination"] != "trust" || p["invoice_id"] != nil || e.invoiceCents(inv, "amount_paid_cents") != 0 {
		t.Fatalf("payment %+v", p)
	}
	if got := e.text(`SELECT invoice_id FROM lawpay_charges`); got != inv {
		t.Fatalf("charge names %q", got)
	}
	e.setting("invoice_pay_destination", "operating")
	inv2, total2 := e.invoice(e.matter, 3)
	_, r = e.pay(coretest.ClientRole, map[string]any{"purpose": "invoice_pay", "invoice_id": inv2}, fake.CardToken)
	if p := e.payment(r.str(t, "payment_id")); p["destination"] != "operating" || p["invoice_id"] != inv2 || e.invoiceCents(inv2, "balance_due_cents") != 0 || total2 == 0 {
		t.Fatalf("operating payment %+v", p)
	}
}

// MOD-PAY-13: a client can't pay another matter's deposit or another client's invoice,
// or use another client's replenishment request; each answers 404 with no write or
// charge.
func TestCoreModPay13(t *testing.T) {
	e := newCoreEnv(t, "CORE-10", "CORE-12", "CORE-23")
	other, otherContact := e.otherMatter("Rowe")
	e.api(http.MethodPost, "/api/matters/"+other+"/deposits", coretest.Staff, map[string]bool{"enabled": true})
	e.api(http.MethodPost, "/api/matters/"+e.matter+"/deposits", coretest.Staff, map[string]bool{"enabled": true})
	inv, _ := e.invoice(other, 4)
	rrOther := e.replenish(other, otherContact, 20000)
	rrWrongMatter := e.replenish(other, e.client, 20000)
	writes := e.tr.writes()
	for _, q := range []url.Values{
		{"purpose": {"trust_deposit"}, "matter_id": {other}, "amount": {"100.00"}},
		{"purpose": {"invoice_pay"}, "invoice_id": {inv}},
		{"purpose": {"trust_deposit"}, "matter_id": {e.matter}, "replenishment_request_id": {rrOther}},
		{"purpose": {"trust_deposit"}, "matter_id": {e.matter}, "replenishment_request_id": {rrWrongMatter}},
	} {
		if r := e.form("/client/pay", coretest.ClientRole, q); r.status != http.StatusNotFound {
			t.Fatalf("%v: %d %s", q, r.status, r.body)
		}
	}
	if e.tr.writes() != writes || e.lp.Calls() != 0 || e.count(`SELECT count(*) FROM lawpay_charges`) != 0 {
		t.Fatal("something was written or charged")
	}
}

// MOD-PAY-14: createPayment fails after the charge: the payer sees the LawPay id, the
// row is charged_unrecorded, the sweep retries with the same key, and after retry_limit
// one exception and one notification to the admins and the responsible attorney.
func TestCoreModPay14(t *testing.T) {
	e := newCoreEnv(t, "CORE-01", "CORE-02", "CORE-03", "CORE-23")
	e.tr.failNext(http.MethodPost, "/api/v1/payments", 4)
	_, r := e.pay(coretest.Staff, map[string]any{"matter_id": e.matter, "amount_cents": 8800}, fake.CardToken)
	lp := r.str(t, "lawpay_charge_id")
	if r.str(t, "state") != "charged_unrecorded" || !strings.Contains(r.str(t, "message"), lp) || !strings.Contains(r.str(t, "message"), "contact the firm") {
		t.Fatalf("result %s", r.body)
	}
	for i := 0; i < 3; i++ {
		if _, err := e.server().Sweep(e.ctx); err != nil {
			t.Fatal(err)
		}
		if n := e.count(`SELECT count(*) FROM exceptions WHERE kind = 'charged_unrecorded'`); (i < 2 && n != 0) || (i == 2 && n != 1) {
			t.Fatalf("sweep %d: exceptions %d", i+1, n)
		}
	}
	keys := map[string]bool{}
	n := 0
	for _, c := range e.tr.of(http.MethodPost, "/api/v1/payments") {
		if c.Path == "/api/v1/payments" {
			n++
			keys[c.Header.Get("Idempotency-Key")] = true
		}
	}
	if n != 4 || len(keys) != 1 {
		t.Fatalf("createPayment calls %d, keys %v", n, keys)
	}
	if got := e.noteUsers(modID + ".exception"); !got[coretest.Admin] || !got[coretest.Attorney] || got[coretest.Staff] || got[coretest.Accountant] {
		t.Fatalf("notified %v", got)
	}
	sent := e.notes(modID + ".exception")
	if _, err := e.server().Sweep(e.ctx); err != nil {
		t.Fatal(err)
	}
	if ps := e.payments(); len(ps) != 1 || ps[0]["reference_number"] != lp {
		t.Fatalf("payments after recovery %+v", ps)
	}
	if e.notes(modID+".exception") != sent || e.lp.ChargeCalls != 1 {
		t.Fatal("notified or charged again")
	}
}
