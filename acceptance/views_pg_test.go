package acceptance

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/CharitonMedia/togavsu/pkg/modulekit/coretest"

	"github.com/CharitonMedia/togavsu-lawpay-docuseal/adapters"
	"github.com/CharitonMedia/togavsu-lawpay-docuseal/fake"
)

// MOD-PAY-48: invoice_payability through an invoice's life.
func TestCoreModPay48(t *testing.T) {
	e := newCoreEnv(t, "CORE-01", "CORE-02", "CORE-07", "CORE-10", "CORE-23")
	inv, total := e.invoice(e.matter, 5)
	if n := e.count(`SELECT count(*) FROM payable_invoices WHERE invoice_id = $1`, inv); n != 1 {
		t.Fatalf("rows %d", n)
	}
	if r := e.oneRow("invoice_payability", inv, coretest.ClientRole); r["payable"] != true || r["unpayable_reason"] != "" {
		t.Fatalf("open %+v", r)
	}
	_, res := e.pay(coretest.ClientRole, map[string]any{"purpose": "invoice_pay", "invoice_id": inv}, fake.BankToken)
	lpID := res.str(t, "lawpay_charge_id")
	if r := e.oneRow("invoice_payability", inv, coretest.ClientRole); r["payable"] != false || r["unpayable_reason"] != "in_progress" || r["in_progress_cents"] != float64(total) {
		t.Fatalf("pending %+v", r)
	}
	e.lp.Settle(lpID, "COMPLETED")
	tx, _ := e.lp.Get(lpID)
	e.hook(fake.ChargeWebhook(tx))
	if r := e.oneRow("invoice_payability", inv, coretest.ClientRole); r["unpayable_reason"] != "in_progress" || r["in_progress_cents"] != float64(total) {
		t.Fatalf("funded %+v", r)
	}
	if r := e.api(http.MethodPost, "/api/invoices/"+inv+"/reopen", coretest.Staff, nil); r.status != http.StatusNoContent {
		t.Fatalf("reopen %d %s", r.status, r.body)
	}
	if r := e.oneRow("invoice_payability", inv, coretest.ClientRole); r["payable"] != true {
		t.Fatalf("reopened %+v", r)
	}
	// Staff apply the trust funds; the core balance reaches zero.
	e.c.Attorney.Must(http.StatusOK, http.MethodPost, "/api/v1/invoices/"+inv+"/apply-trust", map[string]any{"amount_cents": total})
	e.deliver()
	if r := e.oneRow("invoice_payability", inv, coretest.ClientRole); r["unpayable_reason"] != "zero_balance" {
		t.Fatalf("paid %+v", r)
	}
	inv2, _ := e.invoice(e.matter, 2)
	e.setting("lawpay_public_key", "")
	if r := e.oneRow("invoice_payability", inv2, coretest.ClientRole); r["unpayable_reason"] != "not_configured" {
		t.Fatalf("not configured %+v", r)
	}
	e.setting("lawpay_public_key", "pk_synthetic")
	other, _ := e.otherMatter("Riley")
	inv3, _ := e.invoice(other, 2)
	if rows := e.viewRows("invoice_payability", inv3, coretest.ClientRole); len(rows) != 0 {
		t.Fatalf("another client's invoice %+v", rows)
	}
	e.c.Attorney.Must(http.StatusOK, http.MethodPost, "/api/v1/invoices/"+inv2+"/void", map[string]any{"reason": "Synthetic"})
	e.deliver()
	if rows := e.viewRows("invoice_payability", inv2, coretest.ClientRole); len(rows) != 0 {
		t.Fatalf("voided %+v", rows)
	}
}

// signTab opens the client's sign tab as who.
func (e *coreEnv) signTab(who, signing string) resp {
	e.t.Helper()
	return e.page("/client/sign", who, url.Values{"signing_request_id": {signing}, "return_to": {"/portal/client-portal/documents"}})
}

// MOD-PAY-49: signature_requests through a signing, and the sign tab.
func TestCoreModPay49(t *testing.T) {
	e := newCoreEnv(t, "CORE-07", "CORE-09", "CORE-10", "CORE-14")
	_, g := e.sent()
	file, id, sub := g["sent_file_id"].(string), g["id"].(string), e.ds.Last().ID
	status := func() any { return e.oneRow("signature_requests", file, coretest.ClientRole)["status"] }
	if s := status(); s != "awaiting_client" {
		t.Fatalf("sent %v", s)
	}
	tab := e.signTab(coretest.ClientRole, id)
	if tab.status != http.StatusOK || !strings.Contains(string(tab.body), `href="https://docuseal.invalid/s/`) || !strings.Contains(string(tab.body), `target="_blank"`) {
		t.Fatalf("sign tab %d %s", tab.status, tab.body)
	}
	// coretest has one client login, so the other-client 404 stays in the fake-core
	// suite (TestPGModPay49).
	e.dsHook(e.ds.Sign(sub, adapters.RoleClient, e.clock.Now()))
	if s := status(); s != "client_signed" {
		t.Fatalf("client signed %v", s)
	}
	e.dsHook(e.ds.Complete(sub, e.clock.Now().Add(time.Second)))
	if s := status(); s != "completed" {
		t.Fatalf("completed %v", s)
	}
	a3 := e.draft(e.matter, "flat", 90000)
	g3 := e.api(http.MethodPost, "/api/agreements/"+a3+"/send", coretest.Staff, nil).json(t)
	file3 := g3["sent_file_id"].(string)
	var during any
	e.ds.OnArchive = func(string) { during = e.oneRow("signature_requests", file3, coretest.ClientRole)["status"] }
	e.api(http.MethodPost, "/api/signings/"+g3["id"].(string)+"/void", coretest.Staff, map[string]string{"reason": "Withdrawn"})
	e.ds.OnArchive = nil
	if during != "void" || e.oneRow("signature_requests", file3, coretest.ClientRole)["status"] != "void" {
		t.Fatalf("void %v", during)
	}
	e.c.Admin.Must(http.StatusOK, http.MethodPatch, "/api/v1/files/"+file, map[string]any{"shared_with_client": false})
	if rows := e.viewRows("signature_requests", file, coretest.ClientRole); len(rows) != 0 {
		t.Fatalf("unshared %+v", rows)
	}
	if rows := e.viewRows("signature_requests", file, coretest.Staff); len(rows) != 1 {
		t.Fatalf("staff %+v", rows)
	}
}

// MOD-PAY-50: reading a view without its approved scope is forbidden.
func TestCoreModPay50(t *testing.T) {
	e := newCoreEnv(t)
	agent := e.c.AgentToken(coretest.Staff, "matters:read")
	for _, view := range []string{"invoice_payability", "matter_payability", "signature_requests"} {
		if r := agent.Do(http.MethodGet, "/api/v1/modules/"+modID+"/views/"+view+"?record_id=x", nil); r.Code != http.StatusForbidden {
			t.Fatalf("%s: %d %s", view, r.Code, r.Body)
		}
	}
}

// MOD-PAY-51: the awaiting-signature widget counts what the viewer can see; a walled
// matter is absent; the click-through opens the agreements tab filtered by state.
func TestCoreModPay51(t *testing.T) {
	e := newCoreEnv(t, "CORE-07", "CORE-14")
	_, a := e.sent()
	e.dsHook(e.ds.Sign(e.ds.Last().ID, adapters.RoleClient, e.clock.Now()))
	b := e.draft(e.matter, "hourly", 200000)
	e.api(http.MethodPost, "/api/agreements/"+b+"/send", coretest.Staff, nil)
	walled, _ := e.otherMatter("Sato")
	c := e.draft(walled, "flat", 50000)
	e.api(http.MethodPost, "/api/agreements/"+c+"/send", coretest.Staff, nil)
	e.c.Wall(walled, e.c.Attorney)
	counts := func(who string) (any, any) {
		m := e.api(http.MethodGet, "/api/widgets/awaiting-signature", who, nil).json(t)["counts"].(map[string]any)
		return m["sent"], m["client_signed"]
	}
	if s, cs := counts(coretest.Staff); s != 1.0 || cs != 1.0 {
		t.Fatalf("staff %v %v", s, cs)
	}
	if s, cs := counts(coretest.Attorney); s != 2.0 || cs != 1.0 {
		t.Fatalf("attorney %v %v", s, cs)
	}
	w := e.page("/widget/awaiting-signature", coretest.Staff, nil)
	if !strings.Contains(string(w.body), `href="/modules/lawpay-docuseal/agreements?state=sent"`) ||
		!strings.Contains(string(w.body), `href="/modules/lawpay-docuseal/agreements?state=client_signed"`) {
		t.Fatalf("widget %s", w.body)
	}
	list := e.api(http.MethodGet, "/api/agreements?state=client_signed", coretest.Staff, nil).json(t)["data"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["id"] != a["id"] {
		t.Fatalf("filtered %+v", list)
	}
	if tab := e.page("/tab/agreements", coretest.Staff, url.Values{"state": {"sent"}}); tab.status != http.StatusOK || strings.Count(string(tab.body), "Awaiting client signature") != 1 {
		t.Fatalf("tab %s", tab.body)
	}
}

// MOD-PAY-52: matter_payability.
func TestCoreModPay52(t *testing.T) {
	e := newCoreEnv(t)
	if rows := e.viewRows("matter_payability", e.matter, coretest.ClientRole); len(rows) != 0 {
		t.Fatalf("no row expected %+v", rows)
	}
	e.replenish(e.matter, e.client, 50000)
	r := e.oneRow("matter_payability", e.matter, coretest.ClientRole)
	min, _ := strconv.ParseFloat(manifestDefaults(t)["min_charge_cents"], 64)
	if r["trust_deposits_enabled"] != true || r["minimum_deposit_cents"] != min {
		t.Fatalf("on %+v", r)
	}
	if res := e.form("/matter/deposits", coretest.Staff, url.Values{"matter_id": {e.matter}, "enabled": {"false"}}); res.status != http.StatusOK {
		t.Fatalf("off %d %s", res.status, res.body)
	}
	if r := e.oneRow("matter_payability", e.matter, coretest.ClientRole); r["trust_deposits_enabled"] != false {
		t.Fatalf("off %+v", r)
	}
	e.replenish(e.matter, e.client, 10000)
	if r := e.oneRow("matter_payability", e.matter, coretest.ClientRole); r["trust_deposits_enabled"] != false {
		t.Fatalf("re-enabled %+v", r)
	}
	e.form("/matter/deposits", coretest.Staff, url.Values{"matter_id": {e.matter}, "enabled": {"true"}})
	e.setting("core_trust_account_id", e.newTrustAccount(false))
	if r := e.oneRow("matter_payability", e.matter, coretest.ClientRole); r["trust_deposits_enabled"] != false {
		t.Fatalf("trust not ready %+v", r)
	}
	e.setting("core_trust_account_id", e.trustAccount)
	if r := e.oneRow("matter_payability", e.matter, coretest.ClientRole); r["trust_deposits_enabled"] != true {
		t.Fatalf("trust ready again %+v", r)
	}
}

// MOD-PAY-53: decline and expiry.
func TestCoreModPay53(t *testing.T) {
	e := newCoreEnv(t, "CORE-03", "CORE-07", "CORE-14")
	_, g := e.sent()
	file := g["sent_file_id"].(string)
	if !e.ds.Requests[0].ExpireAt.IsZero() || e.oneRow("signature_requests", file, coretest.ClientRole)["expires_at"] != nil {
		t.Fatal("expiry set with signing_expire_days 0")
	}
	e.dsHook(e.ds.Decline(e.ds.Last().ID, e.clock.Now()))
	if e.signingState(g["id"].(string)) != "declined" || e.oneRow("signature_requests", file, coretest.ClientRole)["status"] != "declined" {
		t.Fatalf("declined %s", e.signingState(g["id"].(string)))
	}
	if ex := e.openExceptions("signing_error"); len(ex) != 1 {
		t.Fatalf("exceptions %+v", ex)
	}
	if got := e.noteUsers(modID + ".exception"); !got[coretest.Attorney] {
		t.Fatalf("notified %v", got)
	}
	e.setting("signing_expire_days", "30")
	a2 := e.draft(e.matter, "flat", 90000)
	g2 := e.api(http.MethodPost, "/api/agreements/"+a2+"/send", coretest.Staff, nil).json(t)
	want := e.clock.Now().AddDate(0, 0, 30)
	if got := e.ds.Requests[1].ExpireAt; !got.Equal(want) {
		t.Fatalf("expire_at %v, want %v", got, want)
	}
	file2 := g2["sent_file_id"].(string)
	if exp := e.oneRow("signature_requests", file2, coretest.ClientRole)["expires_at"]; exp != float64(want.Unix()) {
		t.Fatalf("view expires_at %v", exp)
	}
	e.dsHook(e.ds.Expire(e.ds.Last().ID, e.clock.Now()))
	if e.signingState(g2["id"].(string)) != "expired" || e.oneRow("signature_requests", file2, coretest.ClientRole)["status"] != "expired" {
		t.Fatalf("expired %s", e.signingState(g2["id"].(string)))
	}
}
