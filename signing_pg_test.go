package lawpaydocuseal

import (
	"errors"
	"html"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CharitonMedia/togavsu-lawpay-docuseal/adapters"
	"github.com/CharitonMedia/togavsu-lawpay-docuseal/fake"
	"github.com/CharitonMedia/togavsu-lawpay-docuseal/internal/fakecore"
)

func (e *env) draft(matter, feeType string, cents int64) string {
	e.t.Helper()
	body := map[string]any{"matter_id": matter, "fee_type": feeType, "services_description": "Representation in a synthetic dispute.",
		"case_number": "CV-2026-0001", "new_case": true}
	if cents > 0 {
		body["amount_cents"] = cents
	}
	r := e.api(http.MethodPost, "/api/agreements", e.staff, body)
	if r.status != http.StatusOK {
		e.t.Fatalf("draft: %d %s", r.status, r.body)
	}
	return r.str(e.t, "id")
}

// sent drafts and sends an agreement on the env matter and returns the signing.
func (e *env) sent() (agreement string, signing map[string]any) {
	e.t.Helper()
	agreement = e.draft(e.matter.ID, "flat", 150000)
	r := e.api(http.MethodPost, "/api/agreements/"+agreement+"/send", e.staff, nil)
	if r.status != http.StatusOK || r.str(e.t, "state") != "sent" {
		e.t.Fatalf("send: %d %s", r.status, r.body)
	}
	return agreement, r.json(e.t)
}

func (e *env) dsHook(body []byte) resp {
	e.t.Helper()
	return e.docusealHook(e.secrets["docuseal_webhook_token"], body)
}

func (e *env) signingState(id string) string {
	e.t.Helper()
	return e.text(`SELECT state FROM signings WHERE id = $1`, id)
}

// MOD-PAY-34: draft save and its limits; a closed matter is read-only.
func TestPGModPay34(t *testing.T) {
	e := newEnv(t)
	id := e.draft(e.matter.ID, "flat", 150000)
	if fa := e.core.FeeAgreementRecord(id); fa.Status != "draft" || *fa.AmountCents != 150000 || fa.MatterID != e.matter.ID {
		t.Fatalf("agreement %+v", fa)
	}
	if got := e.text(`SELECT case_number FROM agreement_details WHERE fee_agreement_id = $1`, id); got != "CV-2026-0001" {
		t.Fatalf("details %q", got)
	}
	for name, body := range map[string]map[string]any{
		"zero":      {"fee_type": "flat", "amount_cents": 0, "services_description": "x"},
		"too big":   {"fee_type": "flat", "amount_cents": 100000000, "services_description": "x"},
		"no text":   {"fee_type": "flat", "amount_cents": 5000, "services_description": "   "},
		"long text": {"fee_type": "flat", "amount_cents": 5000, "services_description": strings.Repeat("a", 4001)},
	} {
		body["matter_id"] = e.matter.ID
		if r := e.api(http.MethodPost, "/api/agreements", e.staff, body); r.status != http.StatusUnprocessableEntity {
			t.Fatalf("%s: %d %s", name, r.status, r.body)
		}
	}
	if r := e.api(http.MethodPost, "/api/agreements", e.staff, map[string]any{"matter_id": e.matter.ID, "fee_type": "flat",
		"amount_cents": 99999999, "services_description": strings.Repeat("a", 4000)}); r.status != http.StatusOK {
		t.Fatalf("at the limits: %d %s", r.status, r.body)
	}
	e.core.UpdateMatter(e.matter.ID, func(m *fakecore.Matter) { m.Status = "closed" })
	panel := e.page("/matter/fee-agreement", e.staff, url.Values{"matter_id": {e.matter.ID}})
	if panel.status != http.StatusOK || strings.Contains(string(panel.body), "Save draft") || !strings.Contains(string(panel.body), "read-only") {
		t.Fatalf("panel %s", panel.body)
	}
	if r := e.api(http.MethodPost, "/api/agreements", e.staff, map[string]any{"matter_id": e.matter.ID, "fee_type": "flat", "amount_cents": 5000,
		"services_description": "x"}); r.status != http.StatusConflict {
		t.Fatalf("closed: %d %s", r.status, r.body)
	}
}

// MOD-PAY-35: an empty billing type is filled; set ones are kept.
func TestPGModPay35(t *testing.T) {
	e := newEnv(t)
	e.draft(e.matter.ID, "flat", 150000)
	m := e.core.MatterRecord(e.matter.ID)
	if m.BillingType == nil || *m.BillingType != "flat" || m.FlatFeeCents == nil || *m.FlatFeeCents != 150000 {
		t.Fatalf("matter %+v", m)
	}
	// A flat matter with a flat fee keeps it.
	e.draft(e.matter.ID, "flat", 90000)
	if m := e.core.MatterRecord(e.matter.ID); *m.FlatFeeCents != 150000 {
		t.Fatalf("flat fee %d", *m.FlatFeeCents)
	}
	hourly := "hourly"
	other := e.core.AddMatter(e.clientContact.ID, e.attorney.ID)
	e.core.UpdateMatter(other.ID, func(m *fakecore.Matter) { m.BillingType = &hourly })
	e.draft(other.ID, "flat", 150000)
	if m := e.core.MatterRecord(other.ID); *m.BillingType != "hourly" || m.FlatFeeCents != nil {
		t.Fatalf("hourly matter %+v", m)
	}
	blank := e.core.AddMatter(e.clientContact.ID, e.attorney.ID)
	e.draft(blank.ID, "hourly", 250000)
	if m := e.core.MatterRecord(blank.ID); *m.BillingType != "hourly" || m.FlatFeeCents != nil {
		t.Fatalf("blank matter %+v", m)
	}
}

// MOD-PAY-36: Send checks the client's details and portal access before any call.
func TestPGModPay36(t *testing.T) {
	e := newEnv(t)
	id := e.draft(e.matter.ID, "flat", 150000)
	good := *e.clientContact
	e.core.UpdateContact(e.clientContact.ID, func(c *fakecore.Contact) {
		c.Email, c.Phone, c.Line1, c.City, c.Region, c.Postal = "not-an-email", "", "", "", "Texas", "1234"
	})
	writes := e.core.Writes()
	r := e.api(http.MethodPost, "/api/agreements/"+id+"/send", e.staff, nil)
	fields, _ := r.json(t)["fields"].([]any)
	if r.status != http.StatusUnprocessableEntity || len(fields) != 6 {
		t.Fatalf("details: %d %s", r.status, r.body)
	}
	e.core.UpdateContact(e.clientContact.ID, func(c *fakecore.Contact) { *c = good; c.Postal = "62701-1234" })
	for _, remove := range []bool{false, true} {
		e.core.SetAccess(e.clientContact.ID, false, remove)
		r := e.api(http.MethodPost, "/api/agreements/"+id+"/send", e.staff, nil)
		if r.status != http.StatusUnprocessableEntity || r.str(t, "message") != "Give the client portal access first; they sign in the portal." {
			t.Fatalf("access (remove %v): %d %s", remove, r.status, r.body)
		}
	}
	if e.ds.Calls() != 0 || e.core.Writes() != writes || e.count(`SELECT count(*) FROM signings`) != 0 {
		t.Fatal("something was called")
	}
	e.core.SetAccess(e.clientContact.ID, true, false)
	if r := e.api(http.MethodPost, "/api/agreements/"+id+"/send", e.staff, nil); r.str(t, "state") != "sent" {
		t.Fatalf("send with a ZIP+4: %s", r.body)
	}
}

// MOD-PAY-37: Send does every step once.
func TestPGModPay37(t *testing.T) {
	e := newEnv(t)
	agreement, g := e.sent()
	id := g["id"].(string)
	if len(e.ds.Requests) != 1 {
		t.Fatalf("submissions %d", len(e.ds.Requests))
	}
	req := e.ds.Requests[0]
	c := e.clientContact
	want := map[string]map[string]string{
		adapters.RoleClient: {"clientName": c.Given + " " + c.Family, "clientAddress": strings.TrimSpace(c.Line1 + " " + c.Line2),
			"clientCityStateZip": c.City + ", " + c.Region + " " + c.Postal, "clientPhone": c.Phone, "clientEmail": c.Email},
		adapters.RoleFirm: {"representationDescription": "Representation in a synthetic dispute.", "caseNumber": "CV-2026-0001", "newCase": "Yes",
			"feeType": "Flat Fee", "feeAmount": "$1,500.00"},
	}
	for _, sub := range req.Submitters {
		got := map[string]string{}
		for _, f := range sub.Fields {
			got[f.Name] = f.Value
			if sub.Role == adapters.RoleClient && !f.ReadOnly {
				t.Fatalf("client field %s editable", f.Name)
			}
		}
		for k, v := range want[sub.Role] {
			if got[k] != v {
				t.Fatalf("%s %s = %q, want %q", sub.Role, k, got[k], v)
			}
		}
		if sub.Role == adapters.RoleClient && sub.SendEmail {
			t.Fatal("DocuSeal emails the client")
		}
		if sub.Role == adapters.RoleFirm && (sub.Name != "Avery Counsel" || sub.Email != "avery@firm.example.test") {
			t.Fatalf("firm signer %+v", sub)
		}
	}
	if e.text(`SELECT docuseal_submission_id FROM signings WHERE id = $1`, id) != e.ds.Last().ID {
		t.Fatal("ids not stored")
	}
	files := e.core.Files(e.matter.ID)
	if len(files) != 1 || !files[0].Shared || g["sent_file_id"] != files[0].ID {
		t.Fatalf("files %+v, signing %+v", files, g)
	}
	if fa := e.core.FeeAgreementRecord(agreement); fa.Status != "sent" {
		t.Fatalf("agreement %s", fa.Status)
	}
	_, mails, events := e.core.Snapshot()
	if len(mails) != 1 || strings.Join(mails[0].To, ",") != c.ID || mails[0].LinkPath != "/portal/lawpay-docuseal/sign?signing_request_id="+id {
		t.Fatalf("mails %+v", mails)
	}
	if len(events) != 1 || events[0].Summary != "Fee agreement sent for signature" || !events[0].ClientVisible {
		t.Fatalf("events %+v", events)
	}
	// Hourly reads Advance Fee.
	other := e.core.AddMatter(e.clientContact.ID, e.attorney.ID)
	h := e.draft(other.ID, "hourly", 300000)
	e.api(http.MethodPost, "/api/agreements/"+h+"/send", e.staff, nil)
	for _, f := range e.ds.Requests[1].Submitters[1].Fields {
		if f.Name == "feeType" && f.Value != "Advance Fee" {
			t.Fatalf("hourly fee type %q", f.Value)
		}
	}
}

// MOD-PAY-38: two Send clicks at once make one submission.
func TestPGModPay38(t *testing.T) {
	e := newEnv(t)
	id := e.draft(e.matter.ID, "flat", 150000)
	gate := make(chan struct{})
	e.ds.Gate = gate
	first := make(chan resp, 1)
	go func() { first <- e.api(http.MethodPost, "/api/agreements/"+id+"/send", e.staff, nil) }()
	for e.count(`SELECT count(*) FROM signings WHERE state = 'sending'`) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	second := e.api(http.MethodPost, "/api/agreements/"+id+"/send", e.staff, nil)
	close(gate)
	r := <-first
	if second.status != http.StatusConflict || r.str(t, "state") != "sent" || e.ds.CreateCalls != 1 {
		t.Fatalf("second %d %s, first %s, creates %d", second.status, second.body, r.body, e.ds.CreateCalls)
	}
}

// MOD-PAY-39: a failure after the submission archives it and leaves a draft; when the
// archive fails too, the signing is error and an exception opens.
func TestPGModPay39(t *testing.T) {
	e := newEnv(t)
	id := e.draft(e.matter.ID, "flat", 150000)
	boom := func() error { return errors.New("synthetic failure") }
	e.srv.failStoreIDs = boom
	if r := e.api(http.MethodPost, "/api/agreements/"+id+"/send", e.staff, nil); r.status != http.StatusBadGateway {
		t.Fatalf("store ids: %d %s", r.status, r.body)
	}
	if len(e.ds.Archived) != 1 || e.core.FeeAgreementRecord(id).Status != "draft" || e.count(`SELECT count(*) FROM signings`) != 0 {
		t.Fatalf("archived %v", e.ds.Archived)
	}
	e.srv.failStoreIDs = nil
	e.srv.failFileSent = boom
	if r := e.api(http.MethodPost, "/api/agreements/"+id+"/send", e.staff, nil); r.status != http.StatusBadGateway {
		t.Fatalf("file: %d %s", r.status, r.body)
	}
	if len(e.ds.Archived) != 2 || e.core.FeeAgreementRecord(id).Status != "draft" || len(e.core.Files(e.matter.ID)) != 0 {
		t.Fatalf("archived %v", e.ds.Archived)
	}
	e.ds.Script("archive_fail")
	if r := e.api(http.MethodPost, "/api/agreements/"+id+"/send", e.staff, nil); r.status != http.StatusBadGateway {
		t.Fatalf("orphan: %d %s", r.status, r.body)
	}
	orphan := e.ds.Last().ID
	if s, last := e.text(`SELECT state FROM signings`), e.text(`SELECT last_error FROM signings`); s != "error" || !strings.Contains(last, orphan) {
		t.Fatalf("signing %s %q", s, last)
	}
	if ex := e.openExceptions("docuseal_orphan"); len(ex) != 1 || e.core.FeeAgreementRecord(id).Status != "draft" {
		t.Fatalf("exceptions %+v", ex)
	}
	// The agreement can still be sent.
	e.srv.failFileSent = nil
	if r := e.api(http.MethodPost, "/api/agreements/"+id+"/send", e.staff, nil); r.str(t, "state") != "sent" {
		t.Fatalf("resend after error: %s", r.body)
	}
}

// MOD-PAY-40: a failed invite leaves it sent; Resend mails the same link.
func TestPGModPay40(t *testing.T) {
	e := newEnv(t)
	e.core.FailNext("mail_result", 1)
	agreement, g := e.sent()
	id := g["id"].(string)
	if fa := e.core.FeeAgreementRecord(agreement); fa.Status != "sent" || !strings.Contains(g["invite_error"].(string), "Use Resend signing link") {
		t.Fatalf("signing %+v", g)
	}
	panel := e.page("/matter/fee-agreement", e.staff, url.Values{"matter_id": {e.matter.ID}})
	if !strings.Contains(string(panel.body), "Use Resend signing link") || !strings.Contains(string(panel.body), "Resend signing link</button>") {
		t.Fatalf("panel %s", panel.body)
	}
	if r := e.form("/matter/resend", e.staff, url.Values{"signing_id": {id}, "matter_id": {e.matter.ID}}); r.status != http.StatusOK {
		t.Fatalf("resend %d %s", r.status, r.body)
	}
	_, mails, _ := e.core.Snapshot()
	if len(mails) != 1 || mails[0].LinkPath != "/portal/lawpay-docuseal/sign?signing_request_id="+id || e.ds.CreateCalls != 1 {
		t.Fatalf("mails %+v", mails)
	}
	if e.text(`SELECT invite_error FROM signings`) != "" {
		t.Fatal("error kept")
	}
}

// MOD-PAY-41: Void from sent and from client_signed; a client signature during voiding
// is stamped and shown.
func TestPGModPay41(t *testing.T) {
	e := newEnv(t)
	agreement, g := e.sent()
	id := g["id"].(string)
	sub := e.ds.Last().ID
	e.srv.duringVoid = func() {
		if s := e.signingState(id); s != "voiding" {
			t.Errorf("during void: %s", s)
		}
		e.dsHook(e.ds.Sign(sub, adapters.RoleClient, e.clock.Now()))
	}
	r := e.form("/matter/void", e.staff, url.Values{"signing_id": {id}, "reason": {"Terms changed"}, "matter_id": {e.matter.ID}})
	if r.status != http.StatusOK {
		t.Fatalf("void %d %s", r.status, r.body)
	}
	e.srv.duringVoid = nil
	if fa := e.core.FeeAgreementRecord(agreement); fa.Status != "void" || fa.VoidReason != "Terms changed" {
		t.Fatalf("agreement %+v", fa)
	}
	if e.signingState(id) != "voided" || len(e.ds.Archived) != 1 || e.ds.Archived[0] != sub {
		t.Fatalf("state %s archived %v", e.signingState(id), e.ds.Archived)
	}
	_, mails, _ := e.core.Snapshot()
	if len(mails) != 2 || mails[1].Subject != "Your fee agreement was withdrawn" || mails[1].LinkPath != "" {
		t.Fatalf("mails %+v", mails)
	}
	panel := e.page("/matter/fee-agreement", e.staff, url.Values{"matter_id": {e.matter.ID}})
	if !strings.Contains(string(panel.body), "The client signed during voiding") {
		t.Fatalf("panel %s", panel.body)
	}
	// From client_signed.
	a2 := e.draft(e.matter.ID, "flat", 90000)
	g2 := e.api(http.MethodPost, "/api/agreements/"+a2+"/send", e.staff, nil).json(t)
	e.dsHook(e.ds.Sign(e.ds.Last().ID, adapters.RoleClient, e.clock.Now()))
	if s := e.signingState(g2["id"].(string)); s != "client_signed" {
		t.Fatalf("state %s", s)
	}
	if r := e.api(http.MethodPost, "/api/signings/"+g2["id"].(string)+"/void", e.staff, map[string]string{"reason": "Withdrawn"}); r.str(t, "state") != "voided" {
		t.Fatalf("void: %s", r.body)
	}
}

// MOD-PAY-42: a missing or wrong X-Webhook-Token gets 401 and writes nothing.
func TestPGModPay42(t *testing.T) {
	e := newEnv(t)
	e.sent()
	body := e.ds.Sign(e.ds.Last().ID, adapters.RoleClient, e.clock.Now())
	for _, tok := range []string{"", "wrong"} {
		if r := e.docusealHook(tok, body); r.status != http.StatusUnauthorized {
			t.Fatalf("%q: %d", tok, r.status)
		}
	}
	if e.count(`SELECT count(*) FROM docuseal_webhook_receipts`) != 0 || e.text(`SELECT state FROM signings`) != "sent" {
		t.Fatal("written")
	}
}

// MOD-PAY-43: client and firm form.completed, and a duplicate.
func TestPGModPay43(t *testing.T) {
	e := newEnv(t)
	_, g := e.sent()
	id, sub := g["id"].(string), e.ds.Last().ID
	body := e.ds.Sign(sub, adapters.RoleClient, e.clock.Now())
	e.dsHook(body)
	if e.signingState(id) != "client_signed" {
		t.Fatalf("state %s", e.signingState(id))
	}
	e.dsHook(e.ds.Sign(sub, adapters.RoleFirm, e.clock.Now()))
	if e.text(`SELECT countersigned_at::text FROM signings`) == "" {
		t.Fatal("no countersigned_at")
	}
	before := e.text(`SELECT updated_at::text FROM signings`)
	if r := e.dsHook(body); r.str(t, "verdict") != "duplicate" {
		t.Fatalf("%s", r.body)
	}
	if n := e.count(`SELECT max(duplicate_count) FROM docuseal_webhook_receipts`); n != 1 || e.text(`SELECT updated_at::text FROM signings`) != before {
		t.Fatalf("duplicate_count %d", n)
	}
}

// MOD-PAY-44: submission.completed executes once and files one signed PDF.
func TestPGModPay44(t *testing.T) {
	e := newEnv(t)
	agreement, g := e.sent()
	id, sub := g["id"].(string), e.ds.Last().ID
	e.dsHook(e.ds.Sign(sub, adapters.RoleClient, e.clock.Now()))
	// 03:30 UTC on the 14th is the evening of the 13th in Chicago.
	at := time.Date(2026, 11, 14, 3, 30, 0, 0, time.UTC)
	e.ds.Complete(sub, at)
	bodies := [][]byte{e.ds.CompletedBody(sub, at), e.ds.CompletedBody(sub, at.Add(time.Second))}
	var wg sync.WaitGroup
	for _, b := range bodies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.dsHook(b)
		}()
	}
	wg.Wait()
	if n := e.count(`SELECT count(*) FROM docuseal_webhook_receipts WHERE verdict = 'applied' AND event_type = 'submission.completed'`); n != 1 {
		t.Fatalf("applied %d", n)
	}
	var signed []fakecore.File
	for _, f := range e.core.Files(e.matter.ID) {
		if f.ID != g["sent_file_id"] {
			signed = append(signed, f)
		}
	}
	if len(signed) != 1 || !signed[0].Shared || signed[0].Status != "executed" {
		t.Fatalf("signed files %+v", signed)
	}
	fa := e.core.FeeAgreementRecord(agreement)
	if fa.Status != "signed" || fa.SignedOn != "2026-11-13" || fa.SignedFileID != signed[0].ID {
		t.Fatalf("agreement %+v", fa)
	}
	if e.signingState(id) != "executed" || e.text(`SELECT legal_execution_date::text FROM signings`) != "2026-11-13" {
		t.Fatalf("signing %s", e.signingState(id))
	}
	_, _, events := e.core.Snapshot()
	found := false
	for _, ev := range events {
		found = found || (ev.Summary == "Fee agreement executed" && ev.ClientVisible)
	}
	if !found {
		t.Fatalf("events %+v", events)
	}
}

// MOD-PAY-45: submission.completed while still sent proceeds once DocuSeal shows both
// signed; a premature completion is ignored.
func TestPGModPay45(t *testing.T) {
	e := newEnv(t)
	_, g := e.sent()
	id, sub := g["id"].(string), e.ds.Last().ID
	if r := e.dsHook(e.ds.CompletedBody(sub, e.clock.Now())); r.str(t, "verdict") != "ignored" || e.signingState(id) != "sent" {
		t.Fatalf("premature: %s, %s", r.body, e.signingState(id))
	}
	if r := e.dsHook(e.ds.Complete(sub, e.clock.Now().Add(time.Second))); r.str(t, "verdict") != "applied" || e.signingState(id) != "executed" {
		t.Fatalf("complete: %s, %s", r.body, e.signingState(id))
	}
}

// MOD-PAY-46: a PDF failure keeps the signing executed and retries on the sweep.
func TestPGModPay46(t *testing.T) {
	e := newEnv(t)
	_, g := e.sent()
	id, sub := g["id"].(string), e.ds.Last().ID
	e.ds.Script("pdf_fail", "pdf_fail", "pdf_fail", "pdf_fail")
	e.dsHook(e.ds.Complete(sub, e.clock.Now()))
	if e.signingState(id) != "executed" {
		t.Fatalf("state %s", e.signingState(id))
	}
	for i := range 3 {
		e.srv.Sweep(e.ctx)
		if n := len(e.openExceptions("signed_pdf_failed")); (i < 2 && n != 0) || (i == 2 && n != 1) {
			t.Fatalf("sweep %d: exceptions %d", i+1, n)
		}
	}
	notes, _, _ := e.core.Snapshot()
	if len(notes) != 1 || notes[0].Kind != "lawpay-docuseal.exception" {
		t.Fatalf("notifications %+v", notes)
	}
	e.srv.Sweep(e.ctx)
	if e.text(`SELECT signed_file_id FROM signings`) == "" || e.signingState(id) != "executed" || len(e.core.Files(e.matter.ID)) != 2 {
		t.Fatal("not stored after recovery")
	}
}

// MOD-PAY-47: orphan, terminal and internal error.
func TestPGModPay47(t *testing.T) {
	e := newEnv(t)
	if r := e.dsHook(fake.OrphanBody("submission.completed", e.clock.Now())); r.status != http.StatusOK || r.str(t, "verdict") != "orphan" {
		t.Fatalf("orphan %d %s", r.status, r.body)
	}
	_, g := e.sent()
	sub := e.ds.Last().ID
	e.ds.Script("read_fail")
	if r := e.dsHook(e.ds.Complete(sub, e.clock.Now())); r.status != http.StatusInternalServerError {
		t.Fatalf("internal: %d %s", r.status, r.body)
	}
	// DocuSeal's retry of the same body is handled.
	if r := e.dsHook(e.ds.CompletedBody(sub, e.clock.Now())); r.str(t, "verdict") != "applied" || e.signingState(g["id"].(string)) != "executed" {
		t.Fatalf("retry %s", r.body)
	}
	if r := e.dsHook(e.ds.Sign(sub, adapters.RoleFirm, e.clock.Now().Add(time.Minute))); r.status != http.StatusOK || r.str(t, "verdict") != "terminal" {
		t.Fatalf("terminal %d %s", r.status, r.body)
	}
}

// signTab reads the client's sign tab.
func (e *env) signTab(u User, signing string) resp {
	e.t.Helper()
	return e.page("/client/sign", u, url.Values{"signing_request_id": {signing}, "return_to": {"/portal/client-portal/documents"}})
}

func unescaped(r resp) string { return html.UnescapeString(string(r.body)) }
