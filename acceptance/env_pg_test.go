// Package acceptance is this module's Postgres acceptance suite on the real core:
// pkg/modulekit/coretest runs the composed core in the test process, the package is built
// and signed with pkg/modulekit from a generated key, installed and approved as the
// admin, and the module is served in process with the deployment's token, webhook secret
// and database login, with the fake LawPay and DocuSeal adapters and a test clock
// injected. One test per criterion, TestCoreModPayNN. A criterion whose core items
// (docs/modules/CORE-CHANGES.md) the pinned core doesn't build yet skips with the items
// it needs; see internal/coreitems. The fake-core suite in the root package covers the
// same criteria against a stand-in core.
package acceptance

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CharitonMedia/togavsu/pkg/modulekit"
	"github.com/CharitonMedia/togavsu/pkg/modulekit/coretest"

	lawpaydocuseal "github.com/CharitonMedia/togavsu-lawpay-docuseal"
	"github.com/CharitonMedia/togavsu-lawpay-docuseal/fake"
	"github.com/CharitonMedia/togavsu-lawpay-docuseal/internal/coreitems"
)

// modID is the module id.
const modID = lawpaydocuseal.ID

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.b, format+"\n", args...)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func randHex() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// manifestDefaults reads each setting's default from the manifest.
func manifestDefaults(t *testing.T) map[string]string {
	t.Helper()
	files, err := lawpaydocuseal.Files()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	key := ""
	inSettings := false
	for _, line := range strings.Split(string(files[lawpaydocuseal.ManifestFile]), "\n") {
		if strings.HasPrefix(line, "settings:") {
			inSettings = true
			continue
		}
		if inSettings && line != "" && !strings.HasPrefix(line, " ") {
			break
		}
		if !inSettings {
			continue
		}
		tl := strings.TrimSpace(line)
		if strings.HasPrefix(tl, "- key:") {
			key = strings.TrimSpace(strings.TrimPrefix(tl, "- key:"))
			out[key] = ""
		} else if strings.HasPrefix(tl, "default:") && key != "" {
			out[key] = strings.Trim(strings.TrimSpace(strings.TrimPrefix(tl, "default:")), `"`)
		}
	}
	return out
}

// resp is one response from the module or the core's public hook route.
type resp struct {
	status int
	body   []byte
}

func (r resp) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.body, &m); err != nil {
		t.Fatalf("not JSON (%d): %s", r.status, r.body)
	}
	return m
}

func (r resp) str(t *testing.T, k string) string {
	t.Helper()
	v, _ := r.json(t)[k].(string)
	return v
}

// needCore skips t unless the pinned core builds every item the criterion needs and
// every item the install needs.
func needCore(t *testing.T, items ...string) {
	t.Helper()
	if why := coreitems.Skip(items...); why != "" {
		t.Skip(why)
	}
}

// coreCall is one request the module made to the core.
type coreCall struct {
	Method, Path string
	Header       http.Header
}

// failRule fails the next n calls with method whose path is path (exact) or starts with
// prefix and ends with suffix.
type failRule struct {
	method, path, prefix, suffix string
	n                            int
}

func (f *failRule) matches(method, path string) bool {
	if f.n == 0 || f.method != method {
		return false
	}
	if f.path != "" {
		return path == f.path
	}
	return strings.HasPrefix(path, f.prefix) && strings.HasSuffix(path, f.suffix)
}

// coreTransport records the module's calls to the core and can fail the next calls to
// one route, standing in for a core outage the real core can't be told to have.
type coreTransport struct {
	mu    sync.Mutex
	base  string
	calls []coreCall
	rules []*failRule
}

func (c *coreTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !strings.HasPrefix(r.URL.String(), c.base) {
		return http.DefaultTransport.RoundTrip(r)
	}
	c.mu.Lock()
	c.calls = append(c.calls, coreCall{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone()})
	fail := false
	for _, f := range c.rules {
		if f.matches(r.Method, r.URL.Path) {
			f.n--
			fail = true
			break
		}
	}
	c.mu.Unlock()
	if fail {
		if r.Body != nil {
			r.Body.Close()
		}
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"code":"unavailable","message":"synthetic outage"}`)), Request: r}, nil
	}
	return http.DefaultTransport.RoundTrip(r)
}

// failNext makes the next n calls to method and exactly path answer 503.
func (c *coreTransport) failNext(method, path string, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rules = append(c.rules, &failRule{method: method, path: path, n: n})
}

// failNextLike makes the next n calls to method on a path between prefix and suffix
// answer 503.
func (c *coreTransport) failNextLike(method, prefix, suffix string, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rules = append(c.rules, &failRule{method: method, prefix: prefix, suffix: suffix, n: n})
}

// of returns the calls whose method matches and whose path starts with prefix.
func (c *coreTransport) of(method, prefix string) []coreCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []coreCall
	for _, call := range c.calls {
		if call.Method == method && strings.HasPrefix(call.Path, prefix) {
			out = append(out, call)
		}
	}
	return out
}

func (c *coreTransport) writes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, call := range c.calls {
		if call.Method != http.MethodGet && !strings.HasSuffix(call.Path, "/context-tokens/verify") {
			n++
		}
	}
	return n
}

type coreEnv struct {
	t     *testing.T
	ctx   context.Context
	c     *coretest.Core
	lp    *fake.LawPay
	ds    *fake.DocuSeal
	clock *testClock
	tr    *coreTransport
	logs  *logBuf

	mu  sync.Mutex
	srv *lawpaydocuseal.Server
	// mdb is the module's own database login on its own schema.
	mdb *sql.DB

	tokens       map[string]string
	secrets      map[string]string
	trustAccount string
	matter       string
	client       string
}

// newCoreEnv skips unless the pinned core builds items and the install's items, then
// starts the core, installs and approves this module, and configures its settings.
func newCoreEnv(t *testing.T, items ...string) *coreEnv {
	t.Helper()
	needCore(t, items...)
	c := coretest.New(t)
	e := &coreEnv{t: t, ctx: context.Background(), c: c, lp: fake.NewLawPay(), ds: fake.NewDocuSeal(),
		clock: &testClock{t: time.Now().UTC().Truncate(time.Second)}, logs: &logBuf{},
		tr: &coreTransport{base: c.URL}, tokens: map[string]string{}, matter: c.Open, client: c.ClientContact}
	c.Serve(modID, func(m coretest.Module) http.Handler {
		srv := lawpaydocuseal.New(lawpaydocuseal.Config{Core: m.CoreURL, Token: m.Token, WebhookSecret: m.WebhookSecret, DB: m.DB, HTTP: &http.Client{Transport: e.tr},
			LawPay: e.lp, DocuSeal: e.ds, Clock: e.clock, SkewSeconds: 300, Logf: e.logs.Logf})
		e.mu.Lock()
		e.srv = srv
		e.mu.Unlock()
		return srv.Handler()
	})
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signed, _, err := lawpaydocuseal.Build(priv)
	if err != nil {
		t.Fatal(err)
	}
	c.TrustKey("LawPay and DocuSeal test key", pub)
	c.Install(signed)
	c.ApproveAll(modID)
	e.mdb = c.ModuleDB(modID)
	e.configure()
	return e
}

func (e *coreEnv) server() *lawpaydocuseal.Server {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.srv
}

// configure enters every setting as the admin does on the module's settings page, adds
// the client's phone and mailing address, and refreshes the module's state.
func (e *coreEnv) configure() {
	e.t.Helper()
	e.trustAccount = e.newTrustAccount(true)
	e.secrets = map[string]string{"lawpay_secret_key": "sk_" + randHex(), "lawpay_webhook_token": "lwt_" + randHex(),
		"docuseal_api_key": "dsk_" + randHex(), "docuseal_webhook_token": "dwt_" + randHex()}
	st := manifestDefaults(e.t)
	for k, v := range e.secrets {
		st[k] = v
	}
	st["lawpay_public_key"] = "pk_synthetic"
	st["lawpay_operating_account_id"] = "acct_operating"
	st["lawpay_trust_account_id"] = "acct_trust"
	st["core_trust_account_id"] = e.trustAccount
	st["docuseal_base_url"] = "https://docuseal.invalid"
	st["docuseal_template_id"] = "42"
	st["firm_signer_name"] = "Avery Counsel"
	st["firm_signer_email"] = "avery@firm.example.test"
	e.c.Admin.Must(http.StatusOK, http.MethodPut, "/api/v1/modules/"+modID+"/settings", st)
	e.clientDetails(e.client)
	if _, err := e.server().RefreshState(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

// clientDetails gives contact the phone and mailing address Send needs.
func (e *coreEnv) clientDetails(contact string) {
	e.t.Helper()
	e.c.Admin.Must(http.StatusCreated, http.MethodPost, "/api/v1/contact-phones", map[string]any{"contact_id": contact, "label": "mobile",
		"phone": "(217) 555-0142", "primary": true})
	e.c.Admin.Must(http.StatusCreated, http.MethodPost, "/api/v1/contact-addresses", map[string]any{"contact_id": contact, "label": "home",
		"primary": true, "line1": "12 Synthetic Lane", "city": "Springfield", "region": "IL", "postal_code": "62701", "country": "US"})
}

func (e *coreEnv) newTrustAccount(active bool) string {
	e.t.Helper()
	body := map[string]any{"bank": "Synthetic Bank", "name": "IOLTA " + randHex(), "iolta": true, "last_four": "4242", "active": active}
	return jsonStr(e.t, e.c.Admin.Must(http.StatusCreated, http.MethodPost, "/api/v1/trust-accounts", body).JSON, "id")
}

// setting changes one module setting as the admin and refreshes the module's state.
func (e *coreEnv) setting(k, v string) {
	e.t.Helper()
	e.c.Admin.Must(http.StatusOK, http.MethodPut, "/api/v1/modules/"+modID+"/settings", map[string]string{k: v})
	if _, err := e.server().RefreshState(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

// ctxFor is a context token for who, as the core's frames issue it.
func (e *coreEnv) ctxFor(who string) string {
	e.t.Helper()
	if tok, ok := e.tokens[who]; ok {
		return tok
	}
	tok := e.c.ContextFor(who, modID, "")
	e.tokens[who] = tok
	return tok
}

func (e *coreEnv) send(method, target string, headers map[string]string, body []byte) resp {
	e.t.Helper()
	req, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{status: res.StatusCode, body: b}
}

// modURL is the running module's address, as the core's proxy reaches it.
func (e *coreEnv) modURL() string { return e.c.Runtime.BaseURL(modID) }

// api calls the module's own API as who.
func (e *coreEnv) api(method, path, who string, body any) resp {
	e.t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	return e.send(method, e.modURL()+path, map[string]string{coretest.ContextHeader: e.ctxFor(who), "Content-Type": "application/json"}, raw)
}

// page opens a module page as who, with the context token the core's frame passes.
func (e *coreEnv) page(path, who string, q url.Values) resp {
	e.t.Helper()
	if q == nil {
		q = url.Values{}
	}
	q.Set("context", e.ctxFor(who))
	return e.send(http.MethodGet, e.modURL()+path+"?"+q.Encode(), nil, nil)
}

// form posts a module form as who.
func (e *coreEnv) form(path, who string, v url.Values) resp {
	e.t.Helper()
	v.Set("context", e.ctxFor(who))
	return e.send(http.MethodPost, e.modURL()+path, map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, []byte(v.Encode()))
}

// lawpayHook posts a LawPay webhook to the core's public inbound route (CORE-07).
func (e *coreEnv) lawpayHook(token string, body []byte) resp {
	e.t.Helper()
	target := e.c.URL + "/hooks/" + modID + "/lawpay"
	if token != "" {
		target += "?token=" + url.QueryEscape(token)
	}
	return e.send(http.MethodPost, target, map[string]string{"Content-Type": "application/json"}, body)
}

// docusealHook posts a DocuSeal webhook to the core's public inbound route (CORE-07).
func (e *coreEnv) docusealHook(token string, body []byte) resp {
	e.t.Helper()
	h := map[string]string{"Content-Type": "application/json"}
	if token != "" {
		h["X-Webhook-Token"] = token
	}
	return e.send(http.MethodPost, e.c.URL+"/hooks/"+modID+"/docuseal", h, body)
}

// hook posts a LawPay webhook with the right token and returns the verdict.
func (e *coreEnv) hook(body []byte) string {
	e.t.Helper()
	r := e.lawpayHook(e.secrets["lawpay_webhook_token"], body)
	if r.status != http.StatusOK {
		e.t.Fatalf("hook: %d %s", r.status, r.body)
	}
	return r.str(e.t, "verdict")
}

func (e *coreEnv) dsHook(body []byte) resp {
	e.t.Helper()
	return e.docusealHook(e.secrets["docuseal_webhook_token"], body)
}

// count and text query the module's schema through its own database login.
func (e *coreEnv) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.mdb.QueryRow(q, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
	return n
}

func (e *coreEnv) text(q string, args ...any) string {
	e.t.Helper()
	var s sql.NullString
	if err := e.mdb.QueryRow(q, args...).Scan(&s); err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
	return s.String
}

// deliver pumps and posts the core's queued events to the module.
func (e *coreEnv) deliver() { e.c.DeliverEvents() }

// pay creates and submits an intent as who and returns the intent and result bodies.
func (e *coreEnv) pay(who string, req map[string]any, token string) (resp, resp) {
	e.t.Helper()
	in := e.api(http.MethodPost, "/api/pay-intents", who, req)
	if in.status != http.StatusCreated {
		return in, resp{}
	}
	return in, e.api(http.MethodPost, "/api/pay-intents/"+in.str(e.t, "id")+"/submit", who, map[string]string{"token": token})
}

// staffCharge is a staff Take payment on the open matter that must record.
func (e *coreEnv) staffCharge(dest string, cents int64, token string) (chargeID, paymentID, lawpayID string) {
	e.t.Helper()
	in, res := e.pay(coretest.Staff, map[string]any{"matter_id": e.matter, "amount_cents": cents, "destination": dest}, token)
	if in.status != http.StatusCreated || res.status != http.StatusOK {
		e.t.Fatalf("charge: %d %s / %d %s", in.status, in.body, res.status, res.body)
	}
	m := res.json(e.t)
	return m["charge_id"].(string), fmt.Sprint(m["payment_id"]), fmt.Sprint(m["lawpay_charge_id"])
}

// payment reads a core payment as the admin.
func (e *coreEnv) payment(id string) map[string]any {
	e.t.Helper()
	return e.c.Admin.Must(http.StatusOK, http.MethodGet, "/api/v1/payments/"+id, nil).JSON
}

// payments lists every core payment as the admin.
func (e *coreEnv) payments() []map[string]any {
	e.t.Helper()
	return list(e.c.Admin.Must(http.StatusOK, http.MethodGet, "/api/v1/payments", nil).JSON)
}

// trustRows counts core trust ledger rows of entryType ("" for every row).
func (e *coreEnv) trustRows(entryType string) int {
	e.t.Helper()
	return e.c.Count(`SELECT count(*) FROM core.trust_ledger WHERE $1 = '' OR entry_type = $1`, entryType)
}

// notes counts core notifications of kind, one per recipient.
func (e *coreEnv) notes(kind string) int {
	e.t.Helper()
	return e.c.Count(`SELECT count(*) FROM core.notifications WHERE kind = $1`, kind)
}

// noteUsers returns the users a kind of notification went to.
func (e *coreEnv) noteUsers(kind string) map[string]bool {
	e.t.Helper()
	out := map[string]bool{}
	for _, u := range []*coretest.User{e.c.Admin, e.c.Attorney, e.c.Staff, e.c.Accountant} {
		if e.c.Count(`SELECT count(*) FROM core.notifications WHERE kind = $1 AND user_id = $2`, kind, u.ID) > 0 {
			out[u.Role] = true
		}
	}
	return out
}

// invoice bills time on matter, approves and sends the invoice, delivers the events and
// returns the invoice id and its total.
func (e *coreEnv) invoice(matter string, increments int) (string, int64) {
	e.t.Helper()
	e.c.LogTime(coretest.Staff, matter, increments)
	drafts := list(e.c.Staff.Must(http.StatusCreated, http.MethodPost, "/api/v1/invoice-drafts", map[string]any{"matter_id": matter}).JSON)
	if len(drafts) != 1 {
		e.t.Fatalf("drafts %+v", drafts)
	}
	id := drafts[0]["id"].(string)
	e.c.Attorney.Must(http.StatusOK, http.MethodPost, "/api/v1/invoices/"+id+"/approve", nil)
	e.c.Attorney.Must(http.StatusOK, http.MethodPost, "/api/v1/invoices/"+id+"/send", map[string]any{"deliver": "email"})
	e.deliver()
	return id, e.invoiceCents(id, "total_cents")
}

func (e *coreEnv) invoiceCents(id, field string) int64 {
	e.t.Helper()
	v, _ := e.c.Admin.Must(http.StatusOK, http.MethodGet, "/api/v1/invoices/"+id, nil).JSON[field].(float64)
	return int64(v)
}

// replenish has the attorney request a trust replenishment and delivers the event.
func (e *coreEnv) replenish(matter, contact string, cents int64) string {
	e.t.Helper()
	r := e.c.Attorney.Must(http.StatusCreated, http.MethodPost, "/api/v1/trust-replenishment-requests",
		map[string]any{"matter_id": matter, "client_id": contact, "amount_requested_cents": cents})
	e.deliver()
	return jsonStr(e.t, r.JSON, "id")
}

// otherMatter opens a matter for another client contact with a full address, and
// returns the matter and the contact.
func (e *coreEnv) otherMatter(family string) (string, string) {
	e.t.Helper()
	m, contact := e.c.Matter("Estate of "+family, strings.ToLower(family)+"@client.example", nil)
	e.clientDetails(contact)
	return m, contact
}

// viewRows reads a published view as who, through an agent token holding only that
// view's scope.
func (e *coreEnv) viewRows(view, record, who string) []map[string]any {
	e.t.Helper()
	agent := e.c.AgentToken(who, "views:"+modID+"."+view+":read")
	r := agent.Do(http.MethodGet, "/api/v1/modules/"+modID+"/views/"+view+"?record_id="+url.QueryEscape(record), nil)
	if r.Code != http.StatusOK {
		e.t.Fatalf("view %s: %d %s", view, r.Code, r.Body)
	}
	return list(r.JSON)
}

func (e *coreEnv) oneRow(view, record, who string) map[string]any {
	e.t.Helper()
	rows := e.viewRows(view, record, who)
	if len(rows) != 1 {
		e.t.Fatalf("%s %s: %d rows", view, record, len(rows))
	}
	return rows[0]
}

func (e *coreEnv) openExceptions(kind string) []map[string]any {
	e.t.Helper()
	r := e.api(http.MethodGet, "/api/exceptions?kind="+kind, coretest.Attorney, nil)
	var out []map[string]any
	for _, x := range r.json(e.t)["data"].([]any) {
		out = append(out, x.(map[string]any))
	}
	return out
}

// moneyState is what a webhook must not change: core payments and their refunds, and
// the module's charge and refund rows.
func (e *coreEnv) moneyState() string {
	e.t.Helper()
	var b strings.Builder
	for _, p := range e.payments() {
		fmt.Fprintf(&b, "%v%v%v|", p["id"], p["status"], p["refunded_cents"])
	}
	b.WriteString(fmt.Sprint(e.trustRows("")))
	b.WriteString(e.text(`SELECT string_agg(id || state, ',' ORDER BY id) FROM lawpay_charges`))
	b.WriteString(e.text(`SELECT string_agg(id || state, ',' ORDER BY id) FROM lawpay_refunds`))
	return b.String()
}

func (e *coreEnv) refund(who, payment string, cents int64) resp {
	e.t.Helper()
	return e.api(http.MethodPost, "/api/refunds", who, map[string]any{"payment_id": payment, "amount_cents": cents})
}

// refundRows counts a payment's core refunds (core.payment_refunds, CORE-24) in status
// ("" for any status).
func (e *coreEnv) refundRows(payment, status string) int {
	e.t.Helper()
	return e.c.Count(`SELECT count(*) FROM core.payment_refunds WHERE payment_id = $1 AND ($2 = '' OR status = $2)`, payment, status)
}

// refundWithRef reports whether a payment has a completed core refund of cents whose
// reference number is ref.
func (e *coreEnv) refundWithRef(payment, ref string, cents int64) bool {
	e.t.Helper()
	return e.c.Count(`SELECT count(*) FROM core.payment_refunds WHERE payment_id = $1 AND status = 'completed' AND reference_number = $2 AND amount_cents = $3`,
		payment, ref, cents) == 1
}

func (e *coreEnv) draft(matter, feeType string, cents int64) string {
	e.t.Helper()
	body := map[string]any{"matter_id": matter, "fee_type": feeType, "services_description": "Representation in a synthetic dispute.",
		"case_number": "CV-2026-0001", "new_case": true}
	if cents > 0 {
		body["amount_cents"] = cents
	}
	r := e.api(http.MethodPost, "/api/agreements", coretest.Staff, body)
	if r.status != http.StatusOK {
		e.t.Fatalf("draft: %d %s", r.status, r.body)
	}
	return r.str(e.t, "id")
}

// sent drafts and sends an agreement on the open matter and returns the signing.
func (e *coreEnv) sent() (agreement string, signing map[string]any) {
	e.t.Helper()
	agreement = e.draft(e.matter, "flat", 150000)
	r := e.api(http.MethodPost, "/api/agreements/"+agreement+"/send", coretest.Staff, nil)
	if r.status != http.StatusOK || r.str(e.t, "state") != "sent" {
		e.t.Fatalf("send: %d %s", r.status, r.body)
	}
	return agreement, r.json(e.t)
}

func (e *coreEnv) signingState(id string) string {
	e.t.Helper()
	return e.text(`SELECT state FROM signings WHERE id = $1`, id)
}

func (e *coreEnv) feeAgreement(id string) map[string]any {
	e.t.Helper()
	return e.c.Admin.Must(http.StatusOK, http.MethodGet, "/api/v1/fee-agreements/"+id, nil).JSON
}

func (e *coreEnv) matterRecord(id string) map[string]any {
	e.t.Helper()
	return e.c.Admin.Must(http.StatusOK, http.MethodGet, "/api/v1/matters/"+id, nil).JSON
}

// files lists a matter's core files as the admin.
func (e *coreEnv) files(matter string) []map[string]any {
	e.t.Helper()
	return list(e.c.Admin.Must(http.StatusOK, http.MethodGet, "/api/v1/files?matter_id="+url.QueryEscape(matter), nil).JSON)
}

// timeline lists a matter's timeline events as the admin.
func (e *coreEnv) timeline(matter string) []map[string]any {
	e.t.Helper()
	return list(e.c.Admin.Must(http.StatusOK, http.MethodGet, "/api/v1/matters/"+matter+"/timeline", nil).JSON)
}

func (e *coreEnv) noSecrets(where, s string) {
	e.t.Helper()
	for k, v := range e.secrets {
		if strings.Contains(s, v) {
			e.t.Fatalf("%s carries the %s value", where, k)
		}
	}
}

func list(m map[string]any) []map[string]any {
	var out []map[string]any
	data, _ := m["data"].([]any)
	for _, it := range data {
		if row, ok := it.(map[string]any); ok {
			out = append(out, row)
		}
	}
	return out
}

func jsonStr(t *testing.T, m map[string]any, k string) string {
	t.Helper()
	s, ok := m[k].(string)
	if !ok || s == "" {
		t.Fatalf("no %s in %+v", k, m)
	}
	return s
}

// serveBare registers the module with fresh fakes, for the install tests.
func serveBare(c *coretest.Core) {
	c.Serve(modID, func(m coretest.Module) http.Handler {
		return lawpaydocuseal.New(lawpaydocuseal.Config{Core: m.CoreURL, Token: m.Token, WebhookSecret: m.WebhookSecret, DB: m.DB,
			LawPay: fake.NewLawPay(), DocuSeal: fake.NewDocuSeal(), SkewSeconds: 300}).Handler()
	})
}

// signedPackage builds and signs the installable package with a generated key: the
// package without the parts the pinned core refuses (package/pending.yaml).
func signedPackage(t *testing.T) ([]byte, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signed, ip, err := lawpaydocuseal.Build(priv)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range ip.Held {
		t.Logf("%s", h)
	}
	return signed, pub
}

// The core refuses the signed package from an untrusted signer, and with one changed
// byte from a trusted one, before anything runs.
func TestCoreInstallRefusesUntrustedOrTampered(t *testing.T) {
	c := coretest.New(t)
	serveBare(c)
	signed, pub := signedPackage(t)
	if r := c.TryInstall(signed); r.Code != http.StatusUnprocessableEntity || r.JSON["code"] != modulekit.CodeUnsigned {
		t.Fatalf("an untrusted signer: %d %s", r.Code, r.Body)
	}
	c.TrustKey("LawPay and DocuSeal test key", pub)
	var sp modulekit.SignedPackage
	if err := json.Unmarshal(signed, &sp); err != nil {
		t.Fatal(err)
	}
	pkg, err := base64.StdEncoding.DecodeString(sp.PackageBase64)
	if err != nil {
		t.Fatal(err)
	}
	pkg[len(pkg)/2] ^= 0xff
	sp.PackageBase64 = base64.StdEncoding.EncodeToString(pkg)
	tampered, err := json.Marshal(sp)
	if err != nil {
		t.Fatal(err)
	}
	if r := c.TryInstall(tampered); r.Code != http.StatusUnprocessableEntity || r.JSON["code"] != modulekit.CodeTampered {
		t.Fatalf("a changed byte: %d %s", r.Code, r.Body)
	}
	if len(c.Runtime.Deployments()) != 0 || c.SchemaExists(modID) {
		t.Fatal("a refused package ran")
	}
}

// A trusted package, with the parts the pinned core refuses held back, installs, waits for
// approval, deploys with its own token and database login on its own schema, verifies a
// viewer's context, and uninstalls with its data deleted.
func TestCoreInstall(t *testing.T) {
	c := coretest.New(t)
	serveBare(c)
	signed, pub := signedPackage(t)
	c.TrustKey("LawPay and DocuSeal test key", pub)
	if r := c.TryInstall(signed); r.Code != http.StatusAccepted {
		t.Fatalf("install: %d %s", r.Code, r.Body)
	}
	if len(c.Runtime.Deployments()) != 0 || c.SchemaExists(modID) {
		t.Fatal("the module ran before approval")
	}
	c.ApproveAll(modID)
	dep := c.Last(modID)
	if dep.Version != "1.0.0" || dep.Secrets["togavsu_api_token"] == "" || dep.Secrets["togavsu_db_schema"] != "mod_lawpay_docuseal" || !c.SchemaExists(modID) {
		t.Fatalf("deployment %+v", dep)
	}
	mdb := c.ModuleDB(modID)
	if _, err := mdb.Exec(`SELECT count(*) FROM lawpay_charges`); err != nil {
		t.Fatalf("module login on its schema: %v", err)
	}
	if _, err := mdb.Exec(`SELECT 1 FROM core.contacts LIMIT 1`); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("module login read core contacts: %v", err)
	}
	tok := c.ContextFor(coretest.Staff, modID, "")
	if r := c.ModuleClient(modID).Do(http.MethodPost, "/api/v1/modules/"+modID+"/context-tokens/verify", nil, coretest.ContextHeader, tok); r.Code != http.StatusOK || !strings.Contains(r.Body, c.Staff.ID) {
		t.Fatalf("context verify: %d %s", r.Code, r.Body)
	}
	c.Uninstall(modID, true)
	if c.SchemaExists(modID) || len(c.Backups.Made()) != 1 {
		t.Fatalf("uninstall left the schema or took %d backups", len(c.Backups.Made()))
	}
}
