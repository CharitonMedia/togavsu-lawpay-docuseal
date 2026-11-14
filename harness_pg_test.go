package lawpaydocuseal

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/CharitonMedia/togavsu-lawpay-docuseal/fake"
	"github.com/CharitonMedia/togavsu-lawpay-docuseal/internal/fakecore"
)

// testDSN is the Postgres the acceptance tests use. Without it the tests skip locally and
// fail in CI, as the core's harness does.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TOGAVSU_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("TOGAVSU_TEST_DATABASE_URL is required in CI")
		}
		t.Skip("TOGAVSU_TEST_DATABASE_URL not set")
	}
	return dsn
}

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

type env struct {
	t     *testing.T
	ctx   context.Context
	core  *fakecore.Core
	lp    *fake.LawPay
	ds    *fake.DocuSeal
	srv   *Server
	mod   *httptest.Server
	cored *httptest.Server
	db    *sql.DB
	clock *testClock
	logs  *logBuf

	admin, attorney, staff, accountant, client User
	clientContact                          *fakecore.Contact
	matter                                 *fakecore.Matter
	trustAccount                           string
	secrets                                map[string]string
}

// User is a fake core user with a ready context token.
type User struct {
	fakecore.User
	Token string
}

func randHex() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// manifestDefaults reads each setting's default from the manifest.
func manifestDefaults(t *testing.T) map[string]string {
	files, err := Files()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	key := ""
	inSettings := false
	for _, line := range strings.Split(string(files[ManifestFile]), "\n") {
		if strings.HasPrefix(line, "settings:") {
			inSettings = true
			continue
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

var secretSettings = []string{"lawpay_secret_key", "lawpay_webhook_token", "docuseal_api_key", "docuseal_webhook_token"}

func newEnv(t *testing.T) *env {
	t.Helper()
	dsn := testDSN(t)
	ctx := context.Background()
	base, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { base.Close() })
	if err := fakecore.StubSchema(ctx, base); err != nil {
		t.Fatal(err)
	}
	schema := "lpds_t_" + randHex()
	if _, err := base.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		_, _ = base.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
	})
	if err := applyMigrations(db); err != nil {
		t.Fatal(err)
	}
	clock := &testClock{t: time.Now().UTC().Truncate(time.Second)}
	core := fakecore.New(base, clock.Now)
	core.SetViewDB(db)
	cored := httptest.NewServer(core.Handler())
	t.Cleanup(cored.Close)
	e := &env{t: t, ctx: ctx, core: core, lp: fake.NewLawPay(), ds: fake.NewDocuSeal(), db: db, clock: clock, logs: &logBuf{}, cored: cored}
	e.srv = New(Config{Core: cored.URL, Token: core.Token, WebhookSecret: core.Secret, DB: db, LawPay: e.lp, DocuSeal: e.ds,
		Clock: clock, SkewSeconds: 300, Logf: e.logs.Logf})
	e.mod = httptest.NewServer(e.srv.Handler())
	t.Cleanup(e.mod.Close)
	core.ModuleURL = e.mod.URL

	e.trustAccount = core.AddTrustAccount(true)
	e.secrets = map[string]string{"lawpay_secret_key": "sk_" + randHex(), "lawpay_webhook_token": "lwt_" + randHex(),
		"docuseal_api_key": "dsk_" + randHex(), "docuseal_webhook_token": "dwt_" + randHex()}
	st := manifestDefaults(t)
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
	core.SetSettings(st, secretSettings...)
	core.Approve("views:lawpay-docuseal.invoice_payability:read", "views:lawpay-docuseal.matter_payability:read",
		"views:lawpay-docuseal.signature_requests:read")

	mk := func(contact string, roles ...string) User {
		u := core.AddUser(contact, roles...)
		return User{User: u, Token: core.ContextToken(u, "")}
	}
	e.admin, e.attorney, e.staff, e.accountant = mk("", "admin"), mk("", "attorney"), mk("", "staff"), mk("", "accountant")
	e.clientContact = core.AddContact("Jordan", "Client")
	e.client = mk(e.clientContact.ID, "client")
	e.matter = core.AddMatter(e.clientContact.ID, e.attorney.ID)
	if _, err := e.srv.RefreshState(ctx); err != nil {
		t.Fatal(err)
	}
	return e
}

func applyMigrations(db *sql.DB) error {
	files, err := Files()
	if err != nil {
		return err
	}
	var names []string
	for n := range files {
		if strings.HasPrefix(n, "migrations/") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		if _, err := db.Exec(string(files[n])); err != nil {
			return fmt.Errorf("%s: %w", n, err)
		}
	}
	return nil
}

// newClient adds another client with their own matter.
func (e *env) newClient(name string) (User, *fakecore.Matter) {
	ct := e.core.AddContact(name, "Other")
	u := e.core.AddUser(ct.ID, "client")
	m := e.core.AddMatter(ct.ID, e.attorney.ID)
	return User{User: u, Token: e.core.ContextToken(u, "")}, m
}

func (e *env) setting(k, v string) {
	e.core.SetSettings(map[string]string{k: v})
	if _, err := e.srv.RefreshState(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

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

func (e *env) do(method, target string, headers map[string]string, body []byte) resp {
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

// api calls the module's agent API as the viewer.
func (e *env) api(method, path string, u User, body any) resp {
	e.t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	return e.do(method, e.mod.URL+path, map[string]string{"X-Togavsu-Context-Token": u.Token, "Content-Type": "application/json"}, raw)
}

// page opens a module page as the viewer.
func (e *env) page(path string, u User, q url.Values) resp {
	e.t.Helper()
	if q == nil {
		q = url.Values{}
	}
	q.Set("context", u.Token)
	return e.do(http.MethodGet, e.mod.URL+path+"?"+q.Encode(), nil, nil)
}

// form posts a module form as the viewer.
func (e *env) form(path string, u User, v url.Values) resp {
	e.t.Helper()
	v.Set("context", u.Token)
	return e.do(http.MethodPost, e.mod.URL+path, map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, []byte(v.Encode()))
}

// lawpayHook posts a LawPay webhook through the core's public inbound route.
func (e *env) lawpayHook(token string, body []byte) resp {
	e.t.Helper()
	target := e.cored.URL + "/hooks/lawpay-docuseal/lawpay"
	if token != "" {
		target += "?token=" + url.QueryEscape(token)
	}
	return e.do(http.MethodPost, target, map[string]string{"Content-Type": "application/json"}, body)
}

// docusealHook posts a DocuSeal webhook through the core's public inbound route.
func (e *env) docusealHook(token string, body []byte) resp {
	e.t.Helper()
	h := map[string]string{"Content-Type": "application/json"}
	if token != "" {
		h["X-Webhook-Token"] = token
	}
	return e.do(http.MethodPost, e.cored.URL+"/hooks/lawpay-docuseal/docuseal", h, body)
}

func (e *env) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(q, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
	return n
}

func (e *env) text(q string, args ...any) string {
	e.t.Helper()
	var s sql.NullString
	if err := e.db.QueryRow(q, args...).Scan(&s); err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
	return s.String
}

// pay creates and submits an intent and returns the intent and result bodies.
func (e *env) pay(u User, req map[string]any, token string) (resp, resp) {
	e.t.Helper()
	in := e.api(http.MethodPost, "/api/pay-intents", u, req)
	if in.status != http.StatusCreated {
		return in, resp{}
	}
	id := in.str(e.t, "id")
	return in, e.api(http.MethodPost, "/api/pay-intents/"+id+"/submit", u, map[string]string{"token": token})
}

// staffCharge is a staff Take payment that must record.
func (e *env) staffCharge(dest string, cents int64, token string) (chargeID, paymentID, lawpayID string) {
	e.t.Helper()
	in, res := e.pay(e.staff, map[string]any{"matter_id": e.matter.ID, "amount_cents": cents, "destination": dest}, token)
	if in.status != http.StatusCreated || res.status != http.StatusOK {
		e.t.Fatalf("charge: %d %s / %d %s", in.status, in.body, res.status, res.body)
	}
	m := res.json(e.t)
	return m["charge_id"].(string), fmt.Sprint(m["payment_id"]), fmt.Sprint(m["lawpay_charge_id"])
}

// viewRows reads a published view through the core as the viewer.
func (e *env) viewRows(view, record string, u User) []map[string]any {
	e.t.Helper()
	r := e.do(http.MethodGet, e.cored.URL+"/api/v1/modules/lawpay-docuseal/views/"+view+"?record_id="+url.QueryEscape(record),
		map[string]string{"Authorization": "Bearer " + e.core.Token, "X-Togavsu-Context-Token": u.Token}, nil)
	if r.status != http.StatusOK {
		e.t.Fatalf("view %s: %d %s", view, r.status, r.body)
	}
	var out struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.Unmarshal(r.body, &out)
	return out.Data
}

func (e *env) noSecrets(where string, s string) {
	e.t.Helper()
	for k, v := range e.secrets {
		if strings.Contains(s, v) {
			e.t.Fatalf("%s carries the %s value", where, k)
		}
	}
}
