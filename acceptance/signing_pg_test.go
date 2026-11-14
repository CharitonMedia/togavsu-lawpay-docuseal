package acceptance

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CharitonMedia/togavsu/pkg/modulekit/coretest"

	"github.com/CharitonMedia/togavsu-lawpay-docuseal/adapters"
	"github.com/CharitonMedia/togavsu-lawpay-docuseal/fake"
)

// MOD-PAY-34: saving a draft creates the core fee agreement and the agreement_details
// row; the limits are enforced; a closed matter's panel is read-only.
func TestCoreModPay34(t *testing.T) {
	e := newCoreEnv(t, "CORE-22")
	id := e.draft(e.matter, "flat", 150000)
	if fa := e.feeAgreement(id); fa["status"] != "draft" || fa["amount_cents"] != 150000.0 || fa["matter_id"] != e.matter {
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
		body["matter_id"] = e.matter
		if r := e.api(http.MethodPost, "/api/agreements", coretest.Staff, body); r.status != http.StatusUnprocessableEntity {
			t.Fatalf("%s: %d %s", name, r.status, r.body)
		}
	}
	closed, _ := e.otherMatter("Okafor")
	e.c.Attorney.Must(http.StatusOK, http.MethodPost, "/api/v1/matters/"+closed+"/close", map[string]any{"reason": "Concluded", "acknowledge": true})
	panel := e.page("/matter/fee-agreement", coretest.Staff, url.Values{"matter_id": {closed}})
	if panel.status != http.StatusOK || strings.Contains(string(panel.body), "Save draft") || !strings.Contains(string(panel.body), "read-only") {
		t.Fatalf("panel %s", panel.body)
	}
	if r := e.api(http.MethodPost, "/api/agreements", coretest.Staff, map[string]any{"matter_id": closed, "fee_type": "flat", "amount_cents": 5000,
		"services_description": "x"}); r.status != http.StatusConflict {
		t.Fatalf("closed: %d %s", r.status, r.body)
	}
}

// MOD-PAY-35: an empty billing type is filled from the fee type; a set one is kept.
func TestCoreModPay35(t *testing.T) {
	e := newCoreEnv(t)
	blank, _ := e.c.Matter("Estate of Lindqvist", "", map[string]any{"billing_type": nil})
	e.draft(blank, "flat", 150000)
	if m := e.matterRecord(blank); m["billing_type"] != "flat" || m["flat_fee_cents"] != 150000.0 {
		t.Fatalf("blank matter %+v", m)
	}
	e.draft(blank, "flat", 90000)
	if m := e.matterRecord(blank); m["flat_fee_cents"] != 150000.0 {
		t.Fatalf("flat fee overwritten %+v", m)
	}
	// c.Open is hourly.
	e.draft(e.matter, "flat", 150000)
	if m := e.matterRecord(e.matter); m["billing_type"] != "hourly" || m["flat_fee_cents"] != nil {
		t.Fatalf("hourly matter %+v", m)
	}
}

// MOD-PAY-36: Send checks the client's details and portal access before any call.
func TestCoreModPay36(t *testing.T) {
	e := newCoreEnv(t, "CORE-14")
	bare, _ := e.c.Matter("Estate of Ibarra", "", nil)
	// No email, phone or address: six fields. The core refuses malformed values at
	// entry; the fake-core suite covers the module's own format checks.
	id := e.draft(bare, "flat", 150000)
	writes := e.tr.writes()
	r := e.api(http.MethodPost, "/api/agreements/"+id+"/send", coretest.Staff, nil)
	fields, _ := r.json(t)["fields"].([]any)
	if r.status != http.StatusUnprocessableEntity || len(fields) != 6 {
		t.Fatalf("details: %d %s", r.status, r.body)
	}
	// The open matter's client has full details; a client contact with no portal user
	// has no access.
	noAccess, _ := e.otherMatter("Nakamura")
	other := e.draft(noAccess, "flat", 150000)
	r = e.api(http.MethodPost, "/api/agreements/"+other+"/send", coretest.Staff, nil)
	if r.status != http.StatusUnprocessableEntity || r.str(t, "message") != "Give the client portal access first; they sign in the portal." {
		t.Fatalf("access: %d %s", r.status, r.body)
	}
	if e.ds.Calls() != 0 || e.tr.writes() != writes || e.count(`SELECT count(*) FROM signings`) != 0 {
		t.Fatal("something was called")
	}
	if _, g := e.sent(); g["state"] != "sent" {
		t.Fatalf("send with full details: %+v", g)
	}
}

// MOD-PAY-37: Send does every step once.
func TestCoreModPay37(t *testing.T) {
	e := newCoreEnv(t, "CORE-04", "CORE-05", "CORE-14")
	agreement, g := e.sent()
	id := g["id"].(string)
	if len(e.ds.Requests) != 1 {
		t.Fatalf("submissions %d", len(e.ds.Requests))
	}
	want := map[string]map[string]string{
		adapters.RoleClient: {"clientName": "Client Harlow", "clientAddress": "12 Synthetic Lane", "clientCityStateZip": "Springfield, IL 62701",
			"clientPhone": "(217) 555-0142", "clientEmail": "cleo@client.example"},
		adapters.RoleFirm: {"representationDescription": "Representation in a synthetic dispute.", "caseNumber": "CV-2026-0001", "newCase": "Yes",
			"feeType": "Flat Fee", "feeAmount": "$1,500.00"},
	}
	for _, sub := range e.ds.Requests[0].Submitters {
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
		if sub.Role == adapters.RoleFirm && (sub.Name != "Avery Counsel" || sub.Email != "avery@firm.example.test" || !sub.SendEmail) {
			t.Fatalf("firm signer %+v", sub)
		}
	}
	if e.text(`SELECT docuseal_submission_id FROM signings WHERE id = $1`, id) != e.ds.Last().ID {
		t.Fatal("ids not stored")
	}
	files := e.files(e.matter)
	if len(files) != 1 || files[0]["shared_with_client"] != true || g["sent_file_id"] != files[0]["id"] {
		t.Fatalf("files %+v, signing %+v", files, g)
	}
	if fa := e.feeAgreement(agreement); fa["status"] != "sent" {
		t.Fatalf("agreement %+v", fa)
	}
	mails := e.tr.of(http.MethodPost, "/api/v1/mail/messages")
	if len(mails) != 1 || mails[0].Header.Get(coretest.ContextHeader) == "" {
		t.Fatalf("invites %d", len(mails))
	}
	var sentEvent, mailEvent bool
	for _, ev := range e.timeline(e.matter) {
		sentEvent = sentEvent || (ev["summary"] == "Fee agreement sent for signature" && ev["client_visible"] == true)
		mailEvent = mailEvent || ev["action"] == "mail.sent"
	}
	if !sentEvent || !mailEvent {
		t.Fatalf("timeline %+v", e.timeline(e.matter))
	}
	hourly, _ := e.otherMatter("Petrov")
	h := e.draft(hourly, "hourly", 300000)
	e.api(http.MethodPost, "/api/agreements/"+h+"/send", coretest.Staff, nil)
	if len(e.ds.Requests) != 2 {
		t.Fatalf("second submission missing: %d", len(e.ds.Requests))
	}
	for _, sub := range e.ds.Requests[1].Submitters {
		for _, f := range sub.Fields {
			if f.Name == "feeType" && f.Value != "Advance Fee" {
				t.Fatalf("hourly fee type %q", f.Value)
			}
		}
	}
}

// MOD-PAY-38: two Send clicks at once make one submission; the second gets 409.
func TestCoreModPay38(t *testing.T) {
	e := newCoreEnv(t, "CORE-14")
	id := e.draft(e.matter, "flat", 150000)
	gate := make(chan struct{})
	e.ds.Gate = gate
	first := make(chan resp, 1)
	go func() { first <- e.api(http.MethodPost, "/api/agreements/"+id+"/send", coretest.Staff, nil) }()
	for e.count(`SELECT count(*) FROM signings WHERE state = 'sending'`) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	second := e.api(http.MethodPost, "/api/agreements/"+id+"/send", coretest.Staff, nil)
	close(gate)
	r := <-first
	if second.status != http.StatusConflict || r.str(t, "state") != "sent" || e.ds.CreateCalls != 1 {
		t.Fatalf("second %d %s, first %s, creates %d", second.status, second.body, r.body, e.ds.CreateCalls)
	}
}

// MOD-PAY-39: when filing the unsigned copy fails, the submission is archived and the
// agreement stays a draft; when the archive fails too, the signing is error with the
// orphan id and an exception opens. (A failure storing the ids is a module database
// failure, covered by the fake-core suite.)
func TestCoreModPay39(t *testing.T) {
	e := newCoreEnv(t, "CORE-14")
	id := e.draft(e.matter, "flat", 150000)
	e.tr.failNext(http.MethodPost, "/api/v1/files", 1)
	if r := e.api(http.MethodPost, "/api/agreements/"+id+"/send", coretest.Staff, nil); r.status != http.StatusBadGateway {
		t.Fatalf("file: %d %s", r.status, r.body)
	}
	if len(e.ds.Archived) != 1 || e.feeAgreement(id)["status"] != "draft" || len(e.files(e.matter)) != 0 {
		t.Fatalf("archived %v", e.ds.Archived)
	}
	e.tr.failNext(http.MethodPost, "/api/v1/files", 1)
	e.ds.Script("archive_fail")
	if r := e.api(http.MethodPost, "/api/agreements/"+id+"/send", coretest.Staff, nil); r.status != http.StatusBadGateway {
		t.Fatalf("orphan: %d %s", r.status, r.body)
	}
	orphan := e.ds.Last().ID
	if s, last := e.text(`SELECT state FROM signings`), e.text(`SELECT last_error FROM signings`); s != "error" || !strings.Contains(last, orphan) {
		t.Fatalf("signing %s %q", s, last)
	}
	if ex := e.openExceptions("docuseal_orphan"); len(ex) != 1 || e.feeAgreement(id)["status"] != "draft" {
		t.Fatalf("exceptions %+v", ex)
	}
	if r := e.api(http.MethodPost, "/api/agreements/"+id+"/send", coretest.Staff, nil); r.str(t, "state") != "sent" {
		t.Fatalf("send after error: %s", r.body)
	}
}

// MOD-PAY-40: a failed invite leaves the agreement sent with "Use Resend signing link";
// Resend sends the same link with no new submission.
func TestCoreModPay40(t *testing.T) {
	e := newCoreEnv(t, "CORE-04", "CORE-14")
	e.tr.failNext(http.MethodPost, "/api/v1/mail/messages", 1)
	agreement, g := e.sent()
	id := g["id"].(string)
	if fa := e.feeAgreement(agreement); fa["status"] != "sent" || !strings.Contains(g["invite_error"].(string), "Use Resend signing link") {
		t.Fatalf("signing %+v", g)
	}
	panel := e.page("/matter/fee-agreement", coretest.Staff, url.Values{"matter_id": {e.matter}})
	if !strings.Contains(string(panel.body), "Use Resend signing link") || !strings.Contains(string(panel.body), "Resend signing link</button>") {
		t.Fatalf("panel %s", panel.body)
	}
	if r := e.form("/matter/resend", coretest.Staff, url.Values{"signing_id": {id}, "matter_id": {e.matter}}); r.status != http.StatusOK {
		t.Fatalf("resend %d %s", r.status, r.body)
	}
	if n := len(e.tr.of(http.MethodPost, "/api/v1/mail/messages")); n != 2 || e.ds.CreateCalls != 1 || e.text(`SELECT invite_error FROM signings`) != "" {
		t.Fatalf("mail calls %d, submissions %d", n, e.ds.CreateCalls)
	}
}

// MOD-PAY-41: Void from sent and from client_signed; a client signature during voiding
// is stamped and shown.
func TestCoreModPay41(t *testing.T) {
	e := newCoreEnv(t, "CORE-04", "CORE-07", "CORE-14")
	agreement, g := e.sent()
	id, sub := g["id"].(string), e.ds.Last().ID
	e.ds.OnArchive = func(string) {
		if s := e.signingState(id); s != "voiding" {
			t.Errorf("during void: %s", s)
		}
		e.dsHook(e.ds.Sign(sub, adapters.RoleClient, e.clock.Now()))
	}
	if r := e.form("/matter/void", coretest.Staff, url.Values{"signing_id": {id}, "reason": {"Terms changed"}, "matter_id": {e.matter}}); r.status != http.StatusOK {
		t.Fatalf("void %d %s", r.status, r.body)
	}
	e.ds.OnArchive = nil
	if fa := e.feeAgreement(agreement); fa["status"] != "void" || fa["void_reason"] != "Terms changed" {
		t.Fatalf("agreement %+v", fa)
	}
	if e.signingState(id) != "voided" || len(e.ds.Archived) != 1 || e.ds.Archived[0] != sub {
		t.Fatalf("state %s archived %v", e.signingState(id), e.ds.Archived)
	}
	if n := len(e.tr.of(http.MethodPost, "/api/v1/mail/messages")); n != 2 {
		t.Fatalf("mail calls %d, want the invite and the void notice", n)
	}
	if panel := e.page("/matter/fee-agreement", coretest.Staff, url.Values{"matter_id": {e.matter}}); !strings.Contains(string(panel.body), "The client signed during voiding") {
		t.Fatalf("panel %s", panel.body)
	}
	a2 := e.draft(e.matter, "flat", 90000)
	g2 := e.api(http.MethodPost, "/api/agreements/"+a2+"/send", coretest.Staff, nil).json(t)
	e.dsHook(e.ds.Sign(e.ds.Last().ID, adapters.RoleClient, e.clock.Now()))
	if s := e.signingState(g2["id"].(string)); s != "client_signed" {
		t.Fatalf("state %s", s)
	}
	if r := e.api(http.MethodPost, "/api/signings/"+g2["id"].(string)+"/void", coretest.Staff, map[string]string{"reason": "Withdrawn"}); r.str(t, "state") != "voided" {
		t.Fatalf("void: %s", r.body)
	}
}

// MOD-PAY-42: a missing or wrong X-Webhook-Token gets 401 and writes nothing.
func TestCoreModPay42(t *testing.T) {
	e := newCoreEnv(t, "CORE-07", "CORE-14")
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
func TestCoreModPay43(t *testing.T) {
	e := newCoreEnv(t, "CORE-07", "CORE-14")
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

// MOD-PAY-44: submission.completed executes once: one signed PDF filed on the matter,
// shared and executed, the core agreement signed on the firm's local date.
func TestCoreModPay44(t *testing.T) {
	e := newCoreEnv(t, "CORE-01", "CORE-07", "CORE-14", "CORE-25")
	agreement, g := e.sent()
	id, sub := g["id"].(string), e.ds.Last().ID
	e.dsHook(e.ds.Sign(sub, adapters.RoleClient, e.clock.Now()))
	at := time.Date(2026, 11, 14, 3, 30, 0, 0, time.UTC)
	e.ds.Complete(sub, at)
	var wg sync.WaitGroup
	for _, b := range [][]byte{e.ds.CompletedBody(sub, at), e.ds.CompletedBody(sub, at.Add(time.Second))} {
		wg.Add(1)
		go func(b []byte) {
			defer wg.Done()
			e.dsHook(b)
		}(b)
	}
	wg.Wait()
	if n := e.count(`SELECT count(*) FROM docuseal_webhook_receipts WHERE verdict = 'applied' AND event_type = 'submission.completed'`); n != 1 {
		t.Fatalf("applied %d", n)
	}
	var signed []map[string]any
	for _, f := range e.files(e.matter) {
		if f["id"] != g["sent_file_id"] {
			signed = append(signed, f)
		}
	}
	if len(signed) != 1 || signed[0]["shared_with_client"] != true || signed[0]["status"] != "executed" {
		t.Fatalf("signed files %+v", signed)
	}
	zone, _ := e.c.Admin.Must(http.StatusOK, http.MethodGet, "/api/v1/me", nil).JSON["time_zone"].(string)
	loc, err := time.LoadLocation(zone)
	if err != nil {
		loc = time.UTC
	}
	day := at.In(loc).Format("2006-01-02")
	if fa := e.feeAgreement(agreement); fa["status"] != "signed" || fa["signed_on"] != day || fa["signed_file_id"] != signed[0]["id"] {
		t.Fatalf("agreement %+v, want signed on %s", fa, day)
	}
	if e.signingState(id) != "executed" || e.text(`SELECT legal_execution_date::text FROM signings`) != day {
		t.Fatalf("signing %s", e.signingState(id))
	}
	found := false
	for _, ev := range e.timeline(e.matter) {
		found = found || (ev["summary"] == "Fee agreement executed" && ev["client_visible"] == true)
	}
	if !found {
		t.Fatalf("timeline %+v", e.timeline(e.matter))
	}
}

// MOD-PAY-45: submission.completed while still sent proceeds once DocuSeal shows both
// submitters completed; a premature completion is ignored.
func TestCoreModPay45(t *testing.T) {
	e := newCoreEnv(t, "CORE-07", "CORE-14", "CORE-25")
	_, g := e.sent()
	id, sub := g["id"].(string), e.ds.Last().ID
	if r := e.dsHook(e.ds.CompletedBody(sub, e.clock.Now())); r.str(t, "verdict") != "ignored" || e.signingState(id) != "sent" {
		t.Fatalf("premature: %s, %s", r.body, e.signingState(id))
	}
	if r := e.dsHook(e.ds.Complete(sub, e.clock.Now().Add(time.Second))); r.str(t, "verdict") != "applied" || e.signingState(id) != "executed" {
		t.Fatalf("complete: %s, %s", r.body, e.signingState(id))
	}
}

// MOD-PAY-46: a PDF download failure keeps the signing executed, retries on the sweep,
// and after retry_limit opens signed_pdf_failed with a notification.
func TestCoreModPay46(t *testing.T) {
	e := newCoreEnv(t, "CORE-03", "CORE-07", "CORE-14")
	_, g := e.sent()
	id, sub := g["id"].(string), e.ds.Last().ID
	e.ds.Script("pdf_fail", "pdf_fail", "pdf_fail", "pdf_fail")
	e.dsHook(e.ds.Complete(sub, e.clock.Now()))
	if e.signingState(id) != "executed" {
		t.Fatalf("state %s", e.signingState(id))
	}
	for i := 0; i < 3; i++ {
		if _, err := e.server().Sweep(e.ctx); err != nil {
			t.Fatal(err)
		}
		if n := len(e.openExceptions("signed_pdf_failed")); (i < 2 && n != 0) || (i == 2 && n != 1) {
			t.Fatalf("sweep %d: exceptions %d", i+1, n)
		}
	}
	if e.notes(modID+".exception") == 0 {
		t.Fatal("no notification")
	}
	if _, err := e.server().Sweep(e.ctx); err != nil {
		t.Fatal(err)
	}
	if e.text(`SELECT signed_file_id FROM signings`) == "" || e.signingState(id) != "executed" || len(e.files(e.matter)) != 2 {
		t.Fatal("not stored after recovery")
	}
}

// MOD-PAY-47: unknown submissions are orphan, events for finished signings terminal,
// both 200; an internal error answers 500 and DocuSeal's retry is applied.
func TestCoreModPay47(t *testing.T) {
	e := newCoreEnv(t, "CORE-07", "CORE-14")
	if r := e.dsHook(fake.OrphanBody("submission.completed", e.clock.Now())); r.status != http.StatusOK || r.str(t, "verdict") != "orphan" {
		t.Fatalf("orphan %d %s", r.status, r.body)
	}
	_, g := e.sent()
	sub := e.ds.Last().ID
	e.ds.Script("read_fail")
	if r := e.dsHook(e.ds.Complete(sub, e.clock.Now())); r.status != http.StatusInternalServerError {
		t.Fatalf("internal: %d %s", r.status, r.body)
	}
	if r := e.dsHook(e.ds.CompletedBody(sub, e.clock.Now())); r.str(t, "verdict") != "applied" || e.signingState(g["id"].(string)) != "executed" {
		t.Fatalf("retry %s", r.body)
	}
	if r := e.dsHook(e.ds.Sign(sub, adapters.RoleFirm, e.clock.Now().Add(time.Minute))); r.status != http.StatusOK || r.str(t, "verdict") != "terminal" {
		t.Fatalf("terminal %d %s", r.status, r.body)
	}
}
