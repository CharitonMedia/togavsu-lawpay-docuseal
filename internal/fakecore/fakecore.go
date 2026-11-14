// Package fakecore is an in-process stand-in for the Togavsu core API, serving the
// operations this module calls: the current contract plus the CORE items the module
// builds against (CORE-01 service calls, CORE-02 books reads, CORE-03 notifications,
// CORE-04 mail, CORE-07 inbound hooks, CORE-10/12 client reads, CORE-14 client access,
// CORE-22 linkable records, CORE-23 processor payments, CORE-24 refunds and CORE-25 the
// signature callback). It keeps records in memory, mirrors record ids into stub core.*
// tables so the module's ON DELETE RESTRICT keys work, signs event deliveries as the core
// does, and logs every call so tests can assert what the module did and didn't call.
package fakecore

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ModuleID is the module this fake serves.
const ModuleID = "lawpay-docuseal"

// Call is one request the module made.
type Call struct {
	Op, Method, Path, Kind, Idempotency string
}

// User is a Togavsu user.
type User struct {
	ID        string
	Roles     []string
	ContactID string // a client user's linked contact
}

func (u User) has(roles ...string) bool {
	for _, h := range u.Roles {
		for _, r := range roles {
			if h == r {
				return true
			}
		}
	}
	return false
}

func (u User) client() bool {
	return u.has("client") && !u.has("admin", "attorney", "staff", "accountant")
}

// Records.
type (
	Matter struct {
		ID, Number, Name, Status, CaseNumber string
		BillingType                          *string
		FlatFeeCents                         *int64
		ResponsibleAttorney                  string
		ClientContact                        string
	}
	Contact struct {
		ID, Given, Family, Email, Phone    string
		Line1, Line2, City, Region, Postal string
	}
	Invoice struct {
		ID, MatterID, Number, Status string
		TotalCents, PaidCents        int64
	}
	Payment struct {
		ID, ContactID, MatterID, Destination, Method, Reference, Status, Source string
		InvoiceID, TrustAccountID, ReplenishmentID                              string
		AmountCents, RefundedCents                                              int64
		Created                                                                 time.Time
		body                                                                    string
	}
	Refund struct {
		ID, PaymentID, Reason, Reference, Status, Source string
		AmountCents                                      int64
	}
	TrustEntry struct {
		ID, MatterID, ClientID, AccountID, Type, PaymentID string
		Cents                                              int64
	}
	Replenishment struct {
		ID, MatterID, ClientID, Status string
		AmountCents                    int64
	}
	FeeAgreement struct {
		ID, MatterID, FeeType, Description, Status, SignedOn, VoidReason, SignedFileID, SignatureModule string
		AmountCents                                                                                     *int64
	}
	File struct {
		ID, MatterID, Name, Status string
		Shared                     bool
		Data                       []byte
	}
	Notification struct{ Kind, Title, Link, Audience, MatterID string }
	Mail         struct {
		MatterID, Subject, Text, LinkPath string
		To                                []string
	}
	Event struct {
		Action, RecordType, RecordID, MatterID, Summary string
		ClientVisible                                   bool
	}
	TrustFlag struct {
		MatterID     string
		BalanceCents int64
	}
	grant struct {
		Kind, MatterID, ContactID, Destination, InvoiceID, InitID, PaymentID, RefundID string
		AmountCents                                                                    int64
	}
	initialization struct {
		ID, PaymentID string
		grant         grant
		created       bool
	}
)

// Core is the fake core.
type Core struct {
	mu        sync.Mutex
	db        *sql.DB
	viewDB    *sql.DB
	run       string
	seq       int
	Now       func() time.Time
	Token     string // the module's API token
	Secret    string // the module's webhook secret
	ModuleURL string
	HTTP      *http.Client

	settings  map[string]string
	secretKey map[string]bool
	scopes    map[string]bool

	users    map[string]User
	tokens   map[string]tokenClaims
	matters  map[string]*Matter
	walls    map[string]map[string]bool
	contacts map[string]*Contact
	access   map[string]struct {
		UserID string
		Active bool
	}
	invoices  map[string]*Invoice
	payments  map[string]*Payment
	refunds   map[string]*Refund
	trust     []TrustEntry
	accounts  map[string]bool
	repl      map[string]*Replenishment
	fees      map[string]*FeeAgreement
	files     map[string]*File
	uploads   map[string][]byte
	inits     map[string]*initialization
	grants    map[string]grant
	conflicts map[string]string // conflict check id -> matter id, for a no_go check
	idem      map[string]string // idempotency key -> response id
	failNext  map[string]int
	holdGrant bool

	Calls         []Call
	Notifications []Notification
	Mails         []Mail
	Events        []Event
	TrustFlags    []TrustFlag
	Deliveries    []string // event actions delivered to the module
}

type tokenClaims struct {
	User   string
	Matter string
}

// New returns a fake core that mirrors ids into the core.* stub tables on db.
func New(db *sql.DB, now func() time.Time) *Core {
	var b [4]byte
	_, _ = rand.Read(b[:])
	c := &Core{db: db, run: hex.EncodeToString(b[:]), Now: now, HTTP: http.DefaultClient,
		settings: map[string]string{}, secretKey: map[string]bool{}, scopes: map[string]bool{},
		users: map[string]User{}, tokens: map[string]tokenClaims{}, matters: map[string]*Matter{}, walls: map[string]map[string]bool{},
		contacts: map[string]*Contact{}, access: map[string]struct {
			UserID string
			Active bool
		}{}, invoices: map[string]*Invoice{}, payments: map[string]*Payment{}, refunds: map[string]*Refund{},
		accounts: map[string]bool{}, repl: map[string]*Replenishment{}, fees: map[string]*FeeAgreement{}, files: map[string]*File{},
		uploads: map[string][]byte{}, inits: map[string]*initialization{}, grants: map[string]grant{}, conflicts: map[string]string{},
		idem: map[string]string{}, failNext: map[string]int{}}
	c.Token = "module-token-" + c.run
	c.Secret = "webhook-secret-" + c.run
	return c
}

// StubSchema creates the core.* tables the module's foreign keys name, each holding only
// ids. It is safe to call from parallel tests.
func StubSchema(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, stubLockKey); err != nil {
		return err
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, stubLockKey)
	if _, err := conn.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS core`); err != nil {
		return err
	}
	for _, t := range []string{"matters", "contacts", "invoices", "payments", "users", "files", "fee_agreements", "payment_refunds", "trust_accounts"} {
		if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS core.`+t+` (id TEXT PRIMARY KEY)`); err != nil {
			return err
		}
	}
	return nil
}

func (c *Core) mirror(table, id string) {
	if _, err := c.db.Exec(`INSERT INTO core.`+table+` (id) VALUES ($1) ON CONFLICT DO NOTHING`, id); err != nil {
		panic(fmt.Sprintf("fakecore: mirror %s: %v", table, err))
	}
}

func (c *Core) newID(prefix string) string {
	c.seq++
	return fmt.Sprintf("%s_%s_%04d", prefix, c.run, c.seq)
}

// SetViewDB names the module's own schema connection, which the published view reads use.
func (c *Core) SetViewDB(db *sql.DB) { c.viewDB = db }

// ---- setup helpers ----

// SetSettings sets module settings; secret keys are redacted to non-module readers.
func (c *Core) SetSettings(values map[string]string, secret ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range values {
		c.settings[k] = v
	}
	for _, k := range secret {
		c.secretKey[k] = true
	}
}

// Setting reads one setting.
func (c *Core) Setting(k string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.settings[k]
}

// Approve grants the module a scope (used for published view reads).
func (c *Core) Approve(scopes ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range scopes {
		c.scopes[s] = true
	}
}

// Revoke removes an approved scope.
func (c *Core) Revoke(scope string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.scopes, scope)
}

// AddUser adds a user with roles; a client user gets a linked contact.
func (c *Core) AddUser(contact string, roles ...string) User {
	c.mu.Lock()
	defer c.mu.Unlock()
	u := User{ID: c.newID("usr"), Roles: roles, ContactID: contact}
	c.users[u.ID] = u
	c.mirror("users", u.ID)
	if contact != "" {
		c.access[contact] = struct {
			UserID string
			Active bool
		}{u.ID, true}
	}
	return u
}

// ContextToken issues a context token for user, optionally with a matter claim.
func (c *Core) ContextToken(u User, matter string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	tok := c.newID("ctx")
	c.tokens[tok] = tokenClaims{User: u.ID, Matter: matter}
	return tok
}

// AddContact adds a contact with complete DocuSeal details.
func (c *Core) AddContact(given, family string) *Contact {
	c.mu.Lock()
	defer c.mu.Unlock()
	ct := &Contact{ID: c.newID("con"), Given: given, Family: family, Email: strings.ToLower(given) + "@example.test", Phone: "+15555550100",
		Line1: "100 Main St", City: "Springfield", Region: "IL", Postal: "62701"}
	c.contacts[ct.ID] = ct
	c.mirror("contacts", ct.ID)
	return ct
}

// SetAccess sets a contact's portal access; active false deactivates, remove drops it.
func (c *Core) SetAccess(contact string, active, remove bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if remove {
		delete(c.access, contact)
		return
	}
	a := c.access[contact]
	a.Active = active
	c.access[contact] = a
}

// AddMatter adds an open matter with the client and responsible attorney.
func (c *Core) AddMatter(client, attorney string) *Matter {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := &Matter{ID: c.newID("mat"), Number: fmt.Sprintf("2026-%04d", c.seq), Name: "Synthetic matter", Status: "open",
		ResponsibleAttorney: attorney, ClientContact: client}
	c.matters[m.ID] = m
	c.mirror("matters", m.ID)
	return m
}

// MatterRecord returns the matter as stored.
func (c *Core) MatterRecord(id string) Matter {
	c.mu.Lock()
	defer c.mu.Unlock()
	return *c.matters[id]
}

// UpdateMatter changes a matter in place.
func (c *Core) UpdateMatter(id string, f func(*Matter)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f(c.matters[id])
}

// UpdateContact changes a contact in place.
func (c *Core) UpdateContact(id string, f func(*Contact)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f(c.contacts[id])
}

// Wall walls a matter: only the listed users (and no books reader) can see it.
func (c *Core) Wall(matter string, allowed ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := map[string]bool{}
	for _, u := range allowed {
		w[u] = true
	}
	c.walls[matter] = w
}

// AddTrustAccount adds an active (or inactive) trust account.
func (c *Core) AddTrustAccount(active bool) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.newID("tra")
	c.accounts[id] = active
	c.mirror("trust_accounts", id)
	return id
}

// AddInvoice adds a sent invoice of total cents on a matter.
func (c *Core) AddInvoice(matter string, total int64) *Invoice {
	c.mu.Lock()
	defer c.mu.Unlock()
	inv := &Invoice{ID: c.newID("inv"), MatterID: matter, Number: fmt.Sprintf("INV-%04d", c.seq), Status: "sent", TotalCents: total}
	c.invoices[inv.ID] = inv
	c.mirror("invoices", inv.ID)
	return inv
}

// InvoiceRecord returns an invoice as stored.
func (c *Core) InvoiceRecord(id string) Invoice {
	c.mu.Lock()
	defer c.mu.Unlock()
	return *c.invoices[id]
}

// SetInvoice changes an invoice.
func (c *Core) SetInvoice(id string, f func(*Invoice)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f(c.invoices[id])
}

// AddReplenishment adds an active replenishment request.
func (c *Core) AddReplenishment(matter, client string, cents int64) *Replenishment {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := &Replenishment{ID: c.newID("rep"), MatterID: matter, ClientID: client, Status: "active", AmountCents: cents}
	c.repl[r.ID] = r
	return r
}

// ReplenishmentRecord returns a request as stored.
func (c *Core) ReplenishmentRecord(id string) Replenishment {
	c.mu.Lock()
	defer c.mu.Unlock()
	return *c.repl[id]
}

// AddTrustEntry posts a trust ledger row (a deposit is positive).
func (c *Core) AddTrustEntry(matter, client, account, typ string, cents int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.trust = append(c.trust, TrustEntry{ID: c.newID("tle"), MatterID: matter, ClientID: client, AccountID: account, Type: typ, Cents: cents})
}

// TrustEntries returns the ledger rows of a type.
func (c *Core) TrustEntries(typ string) []TrustEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []TrustEntry
	for _, e := range c.trust {
		if typ == "" || e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// AddPayment adds a completed payment made outside the module (for no-go tests).
func (c *Core) AddPayment(matter, contact, method string, cents int64, created time.Time) *Payment {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := &Payment{ID: c.newID("pay"), MatterID: matter, ContactID: contact, Method: method, AmountCents: cents, Destination: "operating",
		Status: "completed", Source: "manual", Created: created}
	c.payments[p.ID] = p
	c.mirror("payments", p.ID)
	return p
}

// PaymentRecord returns a payment as stored.
func (c *Core) PaymentRecord(id string) Payment {
	c.mu.Lock()
	defer c.mu.Unlock()
	return *c.payments[id]
}

// Payments returns every payment.
func (c *Core) Payments() []Payment {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Payment
	for _, p := range c.payments {
		out = append(out, *p)
	}
	return out
}

// RefundsOf returns a payment's refunds.
func (c *Core) RefundsOf(payment string) []Refund {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Refund
	for _, r := range c.refunds {
		if r.PaymentID == payment {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// FeeAgreementRecord returns an agreement as stored.
func (c *Core) FeeAgreementRecord(id string) FeeAgreement {
	c.mu.Lock()
	defer c.mu.Unlock()
	return *c.fees[id]
}

// FileRecord returns a file as stored.
func (c *Core) FileRecord(id string) File {
	c.mu.Lock()
	defer c.mu.Unlock()
	return *c.files[id]
}

// SetFileShared shares or unshares a file with the client.
func (c *Core) SetFileShared(id string, shared bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.files[id].Shared = shared
}

// Files returns every file on a matter.
func (c *Core) Files(matter string) []File {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []File
	for _, f := range c.files {
		if f.MatterID == matter {
			out = append(out, *f)
		}
	}
	return out
}

// AddConflictCheck adds a decided conflict check; noGo makes one hit no_go.
func (c *Core) AddConflictCheck(matter string, noGo bool) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.newID("cfc")
	if noGo {
		c.conflicts[id] = matter
	} else {
		c.conflicts[id] = "clear:" + matter
	}
	return id
}

// FailNext makes the next n calls of op fail with 500.
func (c *Core) FailNext(op string, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failNext[op] += n
}

// HoldGrants stops grant deliveries (the module waits until its deadline).
func (c *Core) HoldGrants(hold bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.holdGrant = hold
}

// CallCount counts calls of op (all calls with op "").
func (c *Core) CallCount(op string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := zero
	for _, call := range c.Calls {
		if op == "" || call.Op == op {
			n++
		}
	}
	return n
}

// Writes counts calls that change something (every method but GET, and not the token
// verify or settings read).
func (c *Core) Writes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := zero
	for _, call := range c.Calls {
		if call.Method != http.MethodGet && call.Op != "verifyContextToken" {
			n++
		}
	}
	return n
}

// CallsOf returns the calls of op.
func (c *Core) CallsOf(op string) []Call {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Call
	for _, call := range c.Calls {
		if call.Op == op {
			out = append(out, call)
		}
	}
	return out
}

// Snapshot returns copies of the recorded notifications, mails and events.
func (c *Core) Snapshot() ([]Notification, []Mail, []Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Notification(nil), c.Notifications...), append([]Mail(nil), c.Mails...), append([]Event(nil), c.Events...)
}

// ---- event delivery ----

// Sign signs a delivery body as the core's Go delivery does.
func Sign(secret string, at time.Time, body []byte) string {
	ts := strconv.FormatInt(at.Unix(), decimalBase)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

type delivery struct {
	body map[string]any
	task string
}

// Emit delivers an event to the module's /events and returns the status. A walled
// matter's money events are dropped for the books reader, as CORE-02 says.
func (c *Core) Emit(action, recordType, recordID, matter string, extra map[string]any) int {
	c.mu.Lock()
	d := c.event(action, recordType, recordID, matter, extra, "")
	walled := matter != "" && c.walls[matter] != nil
	c.mu.Unlock()
	if walled {
		return zero
	}
	return c.deliver(d)
}

// EmitWithID delivers an event with a fixed id (a replay).
func (c *Core) EmitWithID(id, action, recordType, recordID, matter string) int {
	c.mu.Lock()
	d := c.event(action, recordType, recordID, matter, nil, "")
	d.body["id"] = id
	c.mu.Unlock()
	return c.deliver(d)
}

func (c *Core) event(action, recordType, recordID, matter string, extra map[string]any, task string) delivery {
	body := map[string]any{"id": c.newID("evt"), "action": action, "record_type": recordType, "record_id": recordID,
		"matter_id": matter, "occurred_at": c.Now().UTC().Format(time.RFC3339), "module": ModuleID}
	for k, v := range extra {
		body[k] = v
	}
	return delivery{body: body, task: task}
}

func (c *Core) deliver(d delivery) int {
	raw, _ := json.Marshal(d.body)
	req, _ := http.NewRequest(http.MethodPost, c.ModuleURL+"/events", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Togavsu-Signature", Sign(c.Secret, c.Now(), raw))
	if d.task != "" {
		req.Header.Set("X-Togavsu-Task-Authorization", d.task)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return zero
	}
	resp.Body.Close()
	c.mu.Lock()
	c.Deliveries = append(c.Deliveries, fmt.Sprint(d.body["action"]))
	c.mu.Unlock()
	return resp.StatusCode
}

// ---- HTTP ----

type problem struct {
	status           int
	code, message    string
	errorCode, field string
}

func (p *problem) Error() string { return p.code + ": " + p.message }

func perr(status int, code, msg string) *problem {
	return &problem{status: status, code: code, message: msg}
}

var (
	errNotFound  = perr(http.StatusNotFound, "not_found", "Not found.")
	errForbidden = perr(http.StatusForbidden, "forbidden", "Forbidden.")
	errScope     = perr(http.StatusForbidden, "scope_denied", "The operation isn't open to this call.")
)

// caller is who a request runs as.
type caller struct {
	kind   string // viewer, task, service
	user   User
	matter string
	task   string
}

// Handler serves the fake API.
func (c *Core) Handler() http.Handler {
	return http.HandlerFunc(c.serve)
}

func (c *Core) serve(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/hooks/") {
		c.hook(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+c.Token {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "unauthenticated", "message": "bad module token"})
		return
	}
	body, _ := io.ReadAll(r.Body)
	route, params := match(r.Method, r.URL.Path)
	c.mu.Lock()
	call := Call{Op: route.op, Method: r.Method, Path: r.URL.RequestURI(), Idempotency: r.Header.Get("Idempotency-Key")}
	cl, err := c.caller(r)
	call.Kind = cl.kind
	c.Calls = append(c.Calls, call)
	if err == nil && route.fn == nil {
		err = errNotFound
	}
	if err == nil && c.failNext[route.op] > zero {
		c.failNext[route.op]--
		err = perr(http.StatusInternalServerError, "internal", "injected failure")
	}
	var out any
	var status int
	var pending []delivery
	if err == nil {
		req := &request{c: c, r: r, body: body, params: params, caller: cl}
		status, out, err = route.fn(req)
		pending = req.deliveries
	}
	c.mu.Unlock()
	for _, d := range pending {
		c.deliver(d)
	}
	if err != nil {
		p, ok := err.(*problem)
		if !ok {
			p = perr(http.StatusInternalServerError, "internal", err.Error())
		}
		b := map[string]any{"code": p.code, "message": p.message}
		if p.errorCode != "" {
			b["error_code"] = p.errorCode
		}
		if p.field != "" {
			b["field"] = p.field
		}
		writeJSON(w, p.status, b)
		return
	}
	if out == nil {
		w.WriteHeader(status)
		return
	}
	writeJSON(w, status, out)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (c *Core) caller(r *http.Request) (caller, error) {
	if tok := r.Header.Get("X-Togavsu-Context-Token"); tok != "" {
		cl, ok := c.tokens[tok]
		if !ok {
			return caller{kind: "viewer"}, perr(http.StatusUnauthorized, "unauthenticated", "bad context token")
		}
		return caller{kind: "viewer", user: c.users[cl.User], matter: cl.Matter}, nil
	}
	if t := r.Header.Get("X-Togavsu-Task-Authorization"); t != "" {
		return caller{kind: "task", task: t}, nil
	}
	return caller{kind: "service"}, nil
}

// hook is the public inbound route (CORE-07).
func (c *Core) hook(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/hooks/"), "/")
	paths := map[string]string{"lawpay": "/hooks/lawpay", "docuseal": "/hooks/docuseal"}
	if len(parts) != hookParts || parts[hookModule] != ModuleID || paths[parts[hookID]] == "" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(r.Body)
	target := c.ModuleURL + paths[parts[hookID]]
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, _ := http.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	for k, v := range r.Header {
		switch http.CanonicalHeaderKey(k) {
		case "Cookie", "Authorization", "X-Togavsu-Context-Token", "X-Csrf-Token":
			continue
		}
		req.Header[k] = v
	}
	req.Header.Set("X-Togavsu-Module", ModuleID)
	req.Header.Set("X-Togavsu-Inbound-Hook", parts[hookID])
	resp, err := c.HTTP.Do(req)
	if err != nil {
		http.Error(w, "module unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
}
