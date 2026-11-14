package lawpaydocuseal

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/CharitonMedia/togavsu-lawpay-docuseal/adapters"
	"github.com/CharitonMedia/togavsu-lawpay-docuseal/fake"
	"github.com/CharitonMedia/togavsu-lawpay-docuseal/internal/fakecore"
)

func (e *env) oneRow(view, record string, u User) map[string]any {
	e.t.Helper()
	rows := e.viewRows(view, record, u)
	if len(rows) != 1 {
		e.t.Fatalf("%s %s: %d rows", view, record, len(rows))
	}
	return rows[0]
}

// MOD-PAY-48: invoice_payability through an invoice's life.
func TestPGModPay48(t *testing.T) {
	e := newEnv(t)
	inv := e.core.AddInvoice(e.matter.ID, 20000)
	e.core.Emit("invoice.sent", "invoice", inv.ID, e.matter.ID, nil)
	if n := e.count(`SELECT count(*) FROM payable_invoices WHERE invoice_id = $1`, inv.ID); n != 1 {
		t.Fatalf("rows %d", n)
	}
	if r := e.oneRow("invoice_payability", inv.ID, e.client); r["payable"] != true || r["unpayable_reason"] != "" {
		t.Fatalf("open %+v", r)
	}
	// A pending ACH charge naming it (trust destination by default).
	_, res := e.pay(e.client, map[string]any{"purpose": "invoice_pay", "invoice_id": inv.ID}, fake.BankToken)
	lpID := res.str(t, "lawpay_charge_id")
	if r := e.oneRow("invoice_payability", inv.ID, e.client); r["payable"] != false || r["unpayable_reason"] != "in_progress" || r["in_progress_cents"].(float64) != 20000 {
		t.Fatalf("pending %+v", r)
	}
	// It completes in trust for the full balance: funded, still in progress.
	e.lp.Settle(lpID, "COMPLETED")
	tx, _ := e.lp.Get(lpID)
	e.hook(fake.ChargeWebhook(tx))
	if r := e.oneRow("invoice_payability", inv.ID, e.client); r["unpayable_reason"] != "in_progress" || r["in_progress_cents"].(float64) != 20000 {
		t.Fatalf("funded %+v", r)
	}
	if r := e.api(http.MethodPost, "/api/invoices/"+inv.ID+"/reopen", e.staff, nil); r.status != http.StatusNoContent {
		t.Fatalf("reopen %d %s", r.status, r.body)
	}
	if r := e.oneRow("invoice_payability", inv.ID, e.client); r["payable"] != true {
		t.Fatalf("reopened %+v", r)
	}
	// The core balance reaches zero.
	e.core.SetInvoice(inv.ID, func(i *fakecore.Invoice) { i.PaidCents, i.Status = i.TotalCents, "paid" })
	e.core.Emit("invoice.updated", "invoice", inv.ID, e.matter.ID, nil)
	if r := e.oneRow("invoice_payability", inv.ID, e.client); r["unpayable_reason"] != "zero_balance" {
		t.Fatalf("paid %+v", r)
	}
	// LawPay not set up.
	inv2 := e.core.AddInvoice(e.matter.ID, 5000)
	e.core.Emit("invoice.sent", "invoice", inv2.ID, e.matter.ID, nil)
	e.setting("lawpay_public_key", "")
	if r := e.oneRow("invoice_payability", inv2.ID, e.client); r["unpayable_reason"] != "not_configured" {
		t.Fatalf("not configured %+v", r)
	}
	e.setting("lawpay_public_key", "pk_synthetic")
	// Another client's invoice reads no rows.
	other, otherMatter := e.newClient("Riley")
	_ = other
	inv3 := e.core.AddInvoice(otherMatter.ID, 5000)
	e.core.Emit("invoice.sent", "invoice", inv3.ID, otherMatter.ID, nil)
	if rows := e.viewRows("invoice_payability", inv3.ID, e.client); len(rows) != 0 {
		t.Fatalf("other client's invoice %+v", rows)
	}
	e.core.Emit("invoice.voided", "invoice", inv2.ID, e.matter.ID, nil)
	if rows := e.viewRows("invoice_payability", inv2.ID, e.client); len(rows) != 0 {
		t.Fatalf("voided %+v", rows)
	}
}

// MOD-PAY-49: signature_requests through a signing, and the sign tab.
func TestPGModPay49(t *testing.T) {
	e := newEnv(t)
	_, g := e.sent()
	file, id, sub := g["sent_file_id"].(string), g["id"].(string), e.ds.Last().ID
	status := func() any { return e.oneRow("signature_requests", file, e.client)["status"] }
	if s := status(); s != "awaiting_client" {
		t.Fatalf("sent %v", s)
	}
	tab := e.signTab(e.client, id)
	if tab.status != http.StatusOK || !strings.Contains(string(tab.body), `href="https://docuseal.invalid/s/`) || !strings.Contains(string(tab.body), `target="_blank"`) {
		t.Fatalf("sign tab %d %s", tab.status, tab.body)
	}
	other, _ := e.newClient("Quinn")
	if r := e.signTab(other, id); r.status != http.StatusNotFound {
		t.Fatalf("other client: %d", r.status)
	}
	e.dsHook(e.ds.Sign(sub, adapters.RoleClient, e.clock.Now()))
	if s := status(); s != "client_signed" {
		t.Fatalf("client signed %v", s)
	}
	e.dsHook(e.ds.Complete(sub, e.clock.Now().Add(time.Second)))
	if s := status(); s != "completed" {
		t.Fatalf("completed %v", s)
	}
	// Voiding and voided read void.
	a2 := e.draft(e.matter.ID, "flat", 90000)
	g2 := e.api(http.MethodPost, "/api/agreements/"+a2+"/send", e.staff, nil).json(t)
	file2 := g2["sent_file_id"].(string)
	var during any
	e.srv.duringVoid = func() { during = e.oneRow("signature_requests", file2, e.client)["status"] }
	e.api(http.MethodPost, "/api/signings/"+g2["id"].(string)+"/void", e.staff, map[string]string{"reason": "Withdrawn"})
	if during != "void" || e.oneRow("signature_requests", file2, e.client)["status"] != "void" {
		t.Fatalf("void %v", during)
	}
	e.core.SetFileShared(file, false)
	if rows := e.viewRows("signature_requests", file, e.client); len(rows) != 0 {
		t.Fatalf("unshared %+v", rows)
	}
	if rows := e.viewRows("signature_requests", file, e.staff); len(rows) != 1 {
		t.Fatalf("staff %+v", rows)
	}
}

// MOD-PAY-50: a view without its approved scope is forbidden.
func TestPGModPay50(t *testing.T) {
	e := newEnv(t)
	for _, view := range []string{"invoice_payability", "matter_payability", "signature_requests"} {
		e.core.Revoke("views:lawpay-docuseal." + view + ":read")
		r := e.do(http.MethodGet, e.cored.URL+"/api/v1/modules/lawpay-docuseal/views/"+view+"?record_id=x",
			map[string]string{"Authorization": "Bearer " + e.core.Token, "X-Togavsu-Context-Token": e.staff.Token}, nil)
		if r.status != http.StatusForbidden {
			t.Fatalf("%s: %d", view, r.status)
		}
	}
}

// MOD-PAY-51: the awaiting-signature widget counts what the viewer can see.
func TestPGModPay51(t *testing.T) {
	e := newEnv(t)
	_, a := e.sent()
	e.dsHook(e.ds.Sign(e.ds.Last().ID, adapters.RoleClient, e.clock.Now()))
	b := e.draft(e.matter.ID, "hourly", 200000)
	e.api(http.MethodPost, "/api/agreements/"+b+"/send", e.staff, nil)
	walled := e.core.AddMatter(e.clientContact.ID, e.attorney.ID)
	c := e.draft(walled.ID, "flat", 50000)
	e.api(http.MethodPost, "/api/agreements/"+c+"/send", e.staff, nil)
	e.core.Wall(walled.ID, e.attorney.ID)
	counts := func(u User) (any, any) {
		m := e.api(http.MethodGet, "/api/widgets/awaiting-signature", u, nil).json(t)["counts"].(map[string]any)
		return m["sent"], m["client_signed"]
	}
	if s, cs := counts(e.staff); s != 1.0 || cs != 1.0 {
		t.Fatalf("staff %v %v", s, cs)
	}
	if s, cs := counts(e.attorney); s != 2.0 || cs != 1.0 {
		t.Fatalf("attorney %v %v", s, cs)
	}
	w := e.page("/widget/awaiting-signature", e.staff, nil)
	if !strings.Contains(string(w.body), `href="/modules/lawpay-docuseal/agreements?state=sent"`) ||
		!strings.Contains(string(w.body), `href="/modules/lawpay-docuseal/agreements?state=client_signed"`) {
		t.Fatalf("widget %s", w.body)
	}
	list := e.api(http.MethodGet, "/api/agreements?state=client_signed", e.staff, nil).json(t)["data"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["id"] != a["id"] {
		t.Fatalf("filtered %+v", list)
	}
	tab := e.page("/tab/agreements", e.staff, url.Values{"state": {"sent"}})
	if tab.status != http.StatusOK || strings.Count(string(tab.body), "Awaiting client signature") != 1 {
		t.Fatalf("tab %s", tab.body)
	}
}

// MOD-PAY-52: matter_payability.
func TestPGModPay52(t *testing.T) {
	e := newEnv(t)
	if rows := e.viewRows("matter_payability", e.matter.ID, e.client); len(rows) != 0 {
		t.Fatalf("no row expected %+v", rows)
	}
	rr := e.core.AddReplenishment(e.matter.ID, e.clientContact.ID, 50000)
	e.core.Emit("trust.replenishment_requested", "trust_replenishment_request", rr.ID, e.matter.ID, nil)
	r := e.oneRow("matter_payability", e.matter.ID, e.client)
	if r["trust_deposits_enabled"] != true || r["minimum_deposit_cents"].(float64) != mustFloat(e.core.Setting("min_charge_cents")) {
		t.Fatalf("on %+v", r)
	}
	if res := e.form("/matter/deposits", e.staff, url.Values{"matter_id": {e.matter.ID}, "enabled": {"false"}}); res.status != http.StatusOK {
		t.Fatalf("off %d %s", res.status, res.body)
	}
	if r := e.oneRow("matter_payability", e.matter.ID, e.client); r["trust_deposits_enabled"] != false {
		t.Fatalf("off %+v", r)
	}
	// A later request doesn't turn it back on.
	rr2 := e.core.AddReplenishment(e.matter.ID, e.clientContact.ID, 10000)
	e.core.Emit("trust.replenishment_requested", "trust_replenishment_request", rr2.ID, e.matter.ID, nil)
	if r := e.oneRow("matter_payability", e.matter.ID, e.client); r["trust_deposits_enabled"] != false {
		t.Fatalf("re-enabled %+v", r)
	}
	e.form("/matter/deposits", e.staff, url.Values{"matter_id": {e.matter.ID}, "enabled": {"true"}})
	e.setting("core_trust_account_id", e.core.AddTrustAccount(false))
	if r := e.oneRow("matter_payability", e.matter.ID, e.client); r["trust_deposits_enabled"] != false {
		t.Fatalf("trust not ready %+v", r)
	}
	e.setting("core_trust_account_id", e.trustAccount)
	if r := e.oneRow("matter_payability", e.matter.ID, e.client); r["trust_deposits_enabled"] != true {
		t.Fatalf("trust ready again %+v", r)
	}
}

func mustFloat(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

// MOD-PAY-53: decline and expiry.
func TestPGModPay53(t *testing.T) {
	e := newEnv(t)
	_, g := e.sent()
	if !e.ds.Requests[0].ExpireAt.IsZero() || e.oneRow("signature_requests", g["sent_file_id"].(string), e.client)["expires_at"] != nil {
		t.Fatal("expiry set with signing_expire_days 0")
	}
	e.dsHook(e.ds.Decline(e.ds.Last().ID, e.clock.Now()))
	if e.signingState(g["id"].(string)) != "declined" || e.oneRow("signature_requests", g["sent_file_id"].(string), e.client)["status"] != "declined" {
		t.Fatalf("declined %s", e.signingState(g["id"].(string)))
	}
	if ex := e.openExceptions("signing_error"); len(ex) != 1 {
		t.Fatalf("exceptions %+v", ex)
	}
	notes, _, _ := e.core.Snapshot()
	if len(notes) != 1 || notes[0].Audience != "responsible_attorney" {
		t.Fatalf("notifications %+v", notes)
	}
	e.setting("signing_expire_days", "30")
	a2 := e.draft(e.matter.ID, "flat", 90000)
	g2 := e.api(http.MethodPost, "/api/agreements/"+a2+"/send", e.staff, nil).json(t)
	want := e.clock.Now().AddDate(0, 0, 30)
	if got := e.ds.Requests[1].ExpireAt; !got.Equal(want) {
		t.Fatalf("expire_at %v, want %v", got, want)
	}
	file2 := g2["sent_file_id"].(string)
	if exp := e.oneRow("signature_requests", file2, e.client)["expires_at"]; exp != float64(want.Unix()) {
		t.Fatalf("view expires_at %v", exp)
	}
	e.dsHook(e.ds.Expire(e.ds.Last().ID, e.clock.Now()))
	if e.signingState(g2["id"].(string)) != "expired" || e.oneRow("signature_requests", file2, e.client)["status"] != "expired" {
		t.Fatalf("expired %s", e.signingState(g2["id"].(string)))
	}
}
