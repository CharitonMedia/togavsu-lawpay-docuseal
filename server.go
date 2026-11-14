package lawpaydocuseal

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/CharitonMedia/togavsu-lawpay-docuseal/adapters"
	"github.com/CharitonMedia/togavsu-lawpay-docuseal/internal/coreapi"
)

// Clock is the module's time source; tests drive it.
type Clock interface{ Now() time.Time }

// IDs makes record ids; tests may make them predictable.
type IDs interface{ New() string }

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

// uuid7 is a time-ordered random id (UUID version 7).
type uuid7 struct{ clock Clock }

func (u uuid7) New() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	ms := uint64(u.clock.Now().UnixMilli())
	var t [8]byte
	binary.BigEndian.PutUint64(t[:], ms)
	copy(b[:6], t[2:])
	b[6] = b[6]&0x0f | 0x70
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// Config is what the container gets: the core's address, its secrets, its own database
// login, and (in tests) the fake adapters, clock and ids. A nil LawPay or DocuSeal means
// the live adapter, built from the module settings at the moment of each call.
type Config struct {
	Core          string
	Token         string
	WebhookSecret string
	DB            *sql.DB
	HTTP          *http.Client
	LawPay        adapters.LawPay
	DocuSeal      adapters.DocuSeal
	Clock         Clock
	IDs           IDs
	// SkewSeconds is how far a core webhook timestamp may be from the module's clock.
	SkewSeconds int
	// Logf receives operational log lines; nil logs to the standard logger. No line carries
	// a secret, a card token or a client detail.
	Logf func(format string, args ...any)
}

// Server is the module's HTTP server and background worker.
type Server struct {
	c      Config
	core   *coreapi.Client
	grants *waiter

	mu         sync.Mutex
	fileGrants map[string]string // matter id -> background file publication grant

	// test hooks for fault injection; nil in production.
	failStoreIDs func() error
	failFileSent func() error
	duringVoid   func()
}

// New returns the server.
func New(c Config) *Server {
	if c.HTTP == nil {
		c.HTTP = http.DefaultClient
	}
	if c.Clock == nil {
		c.Clock = wallClock{}
	}
	if c.IDs == nil {
		c.IDs = uuid7{clock: c.Clock}
	}
	if c.Logf == nil {
		c.Logf = log.Printf
	}
	return &Server{c: c, grants: newWaiter(), fileGrants: map[string]string{},
		core: &coreapi.Client{Base: c.Core, Token: c.Token, Module: ID, HTTP: c.HTTP}}
}

func (s *Server) now() time.Time { return s.c.Clock.Now().UTC() }

func (s *Server) id() string { return s.c.IDs.New() }

// Handler serves pages, the agent API, core events and provider webhooks.
func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	m.HandleFunc("POST /events", s.events)
	m.HandleFunc("POST /hooks/lawpay", s.lawpayHook)
	m.HandleFunc("GET /hooks/lawpay", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, map[string]bool{"ok": true}) })
	m.HandleFunc("POST /hooks/docuseal", s.docusealHook)
	m.HandleFunc("GET /hooks/docuseal", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, map[string]bool{"ok": true}) })
	s.pageRoutes(m)
	s.apiRoutes(m)
	return m
}

// ---- settings ----

// Settings are the module settings as the module's own token reads them.
type Settings struct {
	LawPaySecret, LawPayPublic, OperatingAccount, TrustAccount, CoreTrustAccount, LawPayBase, LawPayHookToken string
	MinCharge, MaxCharge                                                                                      int64
	InvoiceDestination                                                                                        string
	PayIntentMinutes, AttemptsPerWindow, AttemptWindowMinutes, ReconcileMinutes, RetryLimit, PendingCheckDays int
	DocuSealBase, DocuSealKey, DocuSealHookToken                                                              string
	TemplateID                                                                                                int64
	SignerName, SignerEmail                                                                                   string
	ExpireDays                                                                                                int
	HostedFieldsURL                                                                                           string
}

func atoi(v string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(v))
	return n
}

func atoi64(v string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	return n
}

func parseSettings(m map[string]string) Settings {
	return Settings{
		LawPaySecret: m["lawpay_secret_key"], LawPayPublic: m["lawpay_public_key"], OperatingAccount: m["lawpay_operating_account_id"],
		TrustAccount: m["lawpay_trust_account_id"], CoreTrustAccount: m["core_trust_account_id"], LawPayBase: m["lawpay_api_base"],
		LawPayHookToken: m["lawpay_webhook_token"], MinCharge: atoi64(m["min_charge_cents"]), MaxCharge: atoi64(m["max_charge_cents"]),
		InvoiceDestination: m["invoice_pay_destination"], PayIntentMinutes: atoi(m["pay_intent_minutes"]),
		AttemptsPerWindow: atoi(m["pay_attempts_per_window"]), AttemptWindowMinutes: atoi(m["pay_attempt_window_minutes"]),
		ReconcileMinutes: atoi(m["reconcile_interval_minutes"]), RetryLimit: atoi(m["retry_limit"]),
		PendingCheckDays: atoi(m["pending_check_days"]), DocuSealBase: m["docuseal_base_url"], DocuSealKey: m["docuseal_api_key"],
		DocuSealHookToken: m["docuseal_webhook_token"], TemplateID: atoi64(m["docuseal_template_id"]),
		SignerName: m["firm_signer_name"], SignerEmail: m["firm_signer_email"], ExpireDays: atoi(m["signing_expire_days"]),
		HostedFieldsURL: m["lawpay_hosted_fields_url"],
	}
}

// LawPayReady reports whether every LawPay setting an operating charge needs is set.
func (st Settings) LawPayReady() bool {
	return st.LawPaySecret != "" && st.LawPayPublic != "" && st.OperatingAccount != "" && st.LawPayBase != "" && st.LawPayHookToken != ""
}

// TrustConfigured reports whether the trust settings are filled (the core account's
// activity is checked separately).
func (st Settings) TrustConfigured() bool {
	return st.LawPayReady() && st.TrustAccount != "" && st.CoreTrustAccount != ""
}

// DocuSealReady reports whether every DocuSeal setting is set.
func (st Settings) DocuSealReady() bool {
	return st.DocuSealBase != "" && st.DocuSealKey != "" && st.DocuSealHookToken != "" && st.TemplateID > 0 &&
		st.SignerName != "" && st.SignerEmail != ""
}

// String never prints a secret.
func (st Settings) String() string {
	red := func(v string) string {
		if v == "" {
			return "unset"
		}
		return "redacted"
	}
	return fmt.Sprintf("lawpay_secret_key=%s lawpay_webhook_token=%s docuseal_api_key=%s docuseal_webhook_token=%s operating=%q trust=%q",
		red(st.LawPaySecret), red(st.LawPayHookToken), red(st.DocuSealKey), red(st.DocuSealHookToken), st.OperatingAccount, st.TrustAccount)
}

// GoString keeps %#v from printing secrets too.
func (st Settings) GoString() string { return st.String() }

// settings reads the module settings and records readiness for the published views.
func (s *Server) settings(ctx context.Context) (Settings, error) {
	m, err := s.core.Settings(ctx)
	if err != nil {
		return Settings{}, err
	}
	st := parseSettings(m)
	_, err = s.c.DB.ExecContext(ctx, `INSERT INTO module_state (id, lawpay_ready, trust_ready, docuseal_ready, min_charge_cents, checked_at)
		VALUES (1, $1, false, $2, $3, $4)
		ON CONFLICT (id) DO UPDATE SET lawpay_ready = $1, docuseal_ready = $2, min_charge_cents = $3,
		  trust_ready = module_state.trust_ready AND $5`,
		st.LawPayReady(), st.DocuSealReady(), st.MinCharge, s.now(), st.TrustConfigured())
	return st, err
}

func (s *Server) lawpay(st Settings) adapters.LawPay {
	if s.c.LawPay != nil {
		return s.c.LawPay
	}
	return adapters.NewLawPayHTTP(st.LawPayBase, st.LawPaySecret, s.c.HTTP)
}

func (s *Server) docuseal(st Settings) adapters.DocuSeal {
	if s.c.DocuSeal != nil {
		return s.c.DocuSeal
	}
	return adapters.NewDocuSealHTTP(st.DocuSealBase, st.DocuSealKey, s.c.HTTP)
}

func (s *Server) trustReady(ctx context.Context) bool {
	var ok bool
	_ = s.c.DB.QueryRowContext(ctx, `SELECT trust_ready FROM module_state WHERE id = 1`).Scan(&ok)
	return ok
}

// RefreshState re-reads the settings and checks the core trust account as the books
// reader. A configured trust account the core doesn't return as active blocks trust
// charges and opens account_mismatch with a notification.
func (s *Server) RefreshState(ctx context.Context) (Settings, error) {
	st, err := s.settings(ctx)
	if err != nil {
		return st, err
	}
	ready := false
	if st.TrustConfigured() {
		acct, err := s.core.GetTrustAccount(ctx, coreapi.Service, st.CoreTrustAccount)
		switch {
		case err == nil && acct.Active:
			ready = true
		case err == nil || coreapi.Is(err, http.StatusNotFound):
			s.openException(ctx, exception{Kind: "account_mismatch", TransactionID: "core_trust_account:" + st.CoreTrustAccount,
				Detail: "The Togavsu trust account in the LawPay settings isn't an active trust account. Trust charges are blocked until it is fixed."})
		default:
			return st, err
		}
	}
	_, err = s.c.DB.ExecContext(ctx, `UPDATE module_state SET trust_ready = $1, checked_at = $2 WHERE id = 1`, ready, s.now())
	return st, err
}

// ---- errors and responses ----

// appError is a refusal the payer or staff user sees.
type appError struct {
	Status  int
	Code    string
	Message string
	Fields  []string
	Extra   map[string]any
}

func (e *appError) Error() string { return e.Code + ": " + e.Message }

func fail(status int, code, msg string) *appError {
	return &appError{Status: status, Code: code, Message: msg}
}

var (
	errNotFound   = fail(http.StatusNotFound, "not_found", "Not found.")
	errForbidden  = fail(http.StatusForbidden, "forbidden", "You can't do that.")
	errNoLawPay   = &appError{Status: http.StatusConflict, Code: "not_configured", Message: "LawPay isn't set up.", Extra: map[string]any{"settings_link": settingsLink}}
	errNoDocuSeal = &appError{Status: http.StatusConflict, Code: "not_configured", Message: "DocuSeal isn't set up.", Extra: map[string]any{"settings_link": settingsLink}}
)

// settingsLink is the core's module settings page.
const settingsLink = "/admin?tab=modules"

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// asAppError turns any error into what a caller sees: an appError as is, a core refusal
// with its status and code, and anything else as a 502 with no internal detail.
func asAppError(err error) *appError {
	var ae *appError
	if errors.As(err, &ae) {
		return ae
	}
	var ce *coreapi.Error
	if errors.As(err, &ce) {
		code := ce.Code
		if ce.ErrorCode != "" {
			code = ce.ErrorCode
		}
		return &appError{Status: ce.Status, Code: code, Message: ce.Message}
	}
	return fail(http.StatusBadGateway, "upstream_failed", "Something went wrong. Try again.")
}

func (s *Server) writeErr(w http.ResponseWriter, err error) {
	ae := asAppError(err)
	if ae.Status >= http.StatusInternalServerError {
		s.c.Logf("lawpay-docuseal: %v", err)
	}
	body := map[string]any{"code": ae.Code, "message": ae.Message}
	if len(ae.Fields) > 0 {
		body["fields"] = ae.Fields
	}
	for k, v := range ae.Extra {
		body[k] = v
	}
	writeJSON(w, ae.Status, body)
}

// ---- viewers ----

func contextToken(r *http.Request) string {
	if t := r.Header.Get(coreapi.ContextHeader); t != "" {
		return t
	}
	if t := r.URL.Query().Get("context"); t != "" {
		return t
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		return r.PostFormValue("context")
	}
	return ""
}

// viewer is a verified frame viewer and the token to forward.
type viewer struct {
	coreapi.ViewerInfo
	Token string
}

func (v viewer) auth() coreapi.Auth { return coreapi.Viewer(v.Token) }

// staff reports whether the viewer holds a firm role.
func (v viewer) staff() bool { return v.Has("admin", "attorney", "staff", "accountant") }

// canRefund is the module's own gate on Refund (the core enforces it too).
func (v viewer) canRefund() bool { return v.Has("admin", "attorney") }

// canCharge is who may Take payment.
func (v viewer) canCharge() bool { return v.Has("admin", "attorney", "staff") }

func (s *Server) viewer(r *http.Request) (viewer, error) {
	tok := contextToken(r)
	if tok == "" {
		return viewer{}, fail(http.StatusUnauthorized, "unauthenticated", "Open this page from Togavsu.")
	}
	info, err := s.core.Verify(r.Context(), tok)
	if err != nil {
		if coreapi.Is(err, http.StatusUnauthorized) || coreapi.Is(err, http.StatusForbidden) || coreapi.Is(err, http.StatusNotFound) {
			return viewer{}, fail(http.StatusUnauthorized, "unauthenticated", "Open this page from Togavsu.")
		}
		return viewer{}, err
	}
	return viewer{ViewerInfo: info, Token: tok}, nil
}

// tokenEqual compares a presented token in constant time.
func tokenEqual(got, want string) bool {
	if want == "" || got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// ---- grants ----

// waiter lets a request wait for a grant the core delivers on /events.
type waiter struct {
	mu sync.Mutex
	ch map[string]chan struct{}
}

func newWaiter() *waiter { return &waiter{ch: map[string]chan struct{}{}} }

func (w *waiter) channel(key string) chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	c, ok := w.ch[key]
	if !ok {
		c = make(chan struct{})
		w.ch[key] = c
	}
	return c
}

func (w *waiter) notify(key string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if c, ok := w.ch[key]; ok {
		close(c)
		delete(w.ch, key)
	}
}

var errGrantWait = errors.New("lawpay-docuseal: no grant arrived")

// wait calls found until it reports true, waking on each notify of key, until ctx ends or
// the deadline (the pay intent's own expiry) passes.
func (w *waiter) wait(ctx context.Context, key string, deadline time.Time, found func() (bool, error)) error {
	var timer <-chan time.Time
	if !deadline.IsZero() {
		t := time.NewTimer(time.Until(deadline))
		defer t.Stop()
		timer = t.C
	}
	for {
		c := w.channel(key)
		ok, err := found()
		if err != nil || ok {
			return err
		}
		select {
		case <-c:
		case <-ctx.Done():
			return ctx.Err()
		case <-timer:
			return errGrantWait
		}
	}
}

// ---- exceptions and notifications ----

type exception struct {
	Kind, MatterID, ChargeID, RefundID, SigningID, TransactionID, Detail string
	AmountCents                                                          int64
}

func nullable(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// openException opens one exception per kind and record and notifies the admins (and the
// matter's responsible attorney) once. A second open of the same one does nothing.
func (s *Server) openException(ctx context.Context, e exception) (string, bool) {
	id := s.id()
	res, err := s.c.DB.ExecContext(ctx, `INSERT INTO exceptions (id, kind, matter_id, charge_id, refund_id, signing_id, transaction_id,
		amount_cents, detail, status, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'open', $10) ON CONFLICT DO NOTHING`,
		id, e.Kind, nullable(e.MatterID), nullable(e.ChargeID), nullable(e.RefundID), nullable(e.SigningID), e.TransactionID,
		e.AmountCents, e.Detail, s.now())
	if err != nil {
		s.c.Logf("lawpay-docuseal: exception %s not stored: %v", e.Kind, err)
		return "", false
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", false
	}
	audience := "admins"
	if e.MatterID != "" {
		audience = "admins_and_responsible_attorney"
	}
	if e.Kind == "signing_error" {
		audience = "responsible_attorney"
	}
	link := "/modules/" + ID + "/reconciliation?kind=" + e.Kind
	if err := s.core.Notify(ctx, coreapi.Service, "exception", exceptionTitle(e.Kind), link, audience, e.MatterID); err != nil {
		s.c.Logf("lawpay-docuseal: notification for %s failed: %v", e.Kind, err)
		return id, true
	}
	_, _ = s.c.DB.ExecContext(ctx, `UPDATE exceptions SET notified_at = $2 WHERE id = $1`, id, s.now())
	return id, true
}

func exceptionTitle(kind string) string {
	switch kind {
	case "charged_unrecorded":
		return "A LawPay charge went through but isn't recorded in Togavsu"
	case "external_refund":
		return "A refund was made in the LawPay dashboard"
	case "unknown_transaction":
		return "LawPay has a transaction Togavsu doesn't know"
	case "account_mismatch":
		return "A LawPay account doesn't match the settings"
	case "amount_mismatch":
		return "A LawPay amount doesn't match the charge"
	case "refund_unposted":
		return "A LawPay refund isn't posted in Togavsu"
	case "manual_refund_needed":
		return "A no-go matter needs a manual refund"
	case "pending_ach_stale":
		return "A pending bank payment hasn't settled"
	case "signed_pdf_failed":
		return "The signed fee agreement couldn't be stored"
	case "docuseal_orphan":
		return "A DocuSeal submission was left behind"
	case "signing_error":
		return "A fee agreement signing needs attention"
	}
	return "LawPay and DocuSeal need attention"
}

// eventFor writes a timeline event and logs a failure instead of failing the caller.
func (s *Server) event(ctx context.Context, a coreapi.Auth, action, recordType, recordID, matter, summary string, clientVisible bool) {
	if err := s.core.Event(ctx, a, action, recordType, recordID, matter, summary, clientVisible); err != nil {
		s.c.Logf("lawpay-docuseal: timeline event %s failed: %v", action, err)
	}
}
