package lawpaydocuseal

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/CharitonMedia/togavsu-lawpay-docuseal/internal/coreapi"
)

// Core webhook headers.
const (
	SignatureHeader = "X-Togavsu-Signature"
	TimestampHeader = "X-Togavsu-Timestamp"
)

var errSignature = errors.New("lawpay-docuseal: bad core webhook signature")

// VerifyCoreWebhook checks the core's HMAC-SHA256 signature on an event delivery. It
// accepts the form the core's Go delivery writes, "t=<unix>,v1=<hex>" over t + "." + body,
// and the contract's form, "sha256=<hex>" over the body with the time in
// X-Togavsu-Timestamp. Either way the time must be within skew of now.
func VerifyCoreWebhook(secret, header, timestamp string, body []byte, now time.Time, skew time.Duration) error {
	if secret == "" || header == "" {
		return errSignature
	}
	var ts, sig string
	var signed []byte
	if strings.HasPrefix(header, "sha256=") {
		ts, sig = timestamp, strings.TrimPrefix(header, "sha256=")
		signed = body
	} else {
		for _, part := range strings.Split(header, ",") {
			k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
			switch k {
			case "t":
				ts = v
			case "v1":
				sig = v
			}
		}
		signed = append([]byte(ts+"."), body...)
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return errSignature
	}
	if d := now.Sub(time.Unix(unix, 0)); d > skew || d < -skew {
		return errSignature
	}
	want, err := hex.DecodeString(sig)
	if err != nil {
		return errSignature
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(signed)
	if !hmac.Equal(mac.Sum(nil), want) {
		return errSignature
	}
	return nil
}

// Event is one core event delivery.
type Event struct {
	ID                           string          `json:"id"`
	Action                       string          `json:"action"`
	RecordType                   string          `json:"record_type"`
	RecordID                     string          `json:"record_id"`
	MatterID                     string          `json:"matter_id"`
	OccurredAt                   string          `json:"occurred_at"`
	Module                       string          `json:"module"`
	ProcessorAuthorization       json.RawMessage `json:"processor_authorization"`
	RefundAuthorization          json.RawMessage `json:"refund_authorization"`
	BackgroundFileAuthorization  json.RawMessage `json:"background_file_authorization"`
	Data                         json.RawMessage `json:"data"`
	TaskAuthorizationFromHeader string          `json:"-"`
}

// grant is the delivered grant's binding. The credential itself is opaque.
type grant struct {
	Credential       string `json:"credential"`
	Token            string `json:"token"`
	InitializationID string `json:"initialization_id"`
	PaymentID        string `json:"payment_id"`
	RefundID         string `json:"refund_id"`
	MatterID         string `json:"matter_id"`
	ExpiresAt        string `json:"expires_at"`
}

func parseGrant(raw json.RawMessage, header string) (grant, bool) {
	var g grant
	if len(raw) == 0 || string(raw) == "null" {
		if header == "" {
			return g, false
		}
		g.Credential = header
		return g, true
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		g.Credential = str
	} else if json.Unmarshal(raw, &g) != nil {
		return g, false
	}
	if g.Credential == "" {
		g.Credential = g.Token
	}
	if header != "" {
		g.Credential = header
	}
	return g, g.Credential != ""
}

// events receives the core's signed event deliveries. Each event id is handled once;
// a handler error answers 500 and forgets the id so the core's retry handles it again.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	skew := time.Duration(s.c.SkewSeconds) * time.Second
	if err := VerifyCoreWebhook(s.c.WebhookSecret, r.Header.Get(SignatureHeader), r.Header.Get(TimestampHeader), body, s.now(), skew); err != nil {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	var ev Event
	if err := json.Unmarshal(body, &ev); err != nil || ev.ID == "" {
		http.Error(w, "bad event", http.StatusBadRequest)
		return
	}
	ev.TaskAuthorizationFromHeader = r.Header.Get(coreapi.TaskHeader)
	res, err := s.c.DB.ExecContext(r.Context(), `INSERT INTO received_events (event_id, action, record_id, received_at)
		VALUES ($1, $2, $3, $4) ON CONFLICT (event_id) DO NOTHING`, ev.ID, ev.Action, ev.RecordID, s.now())
	if err != nil {
		http.Error(w, "not stored", http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := s.handleEvent(r.Context(), ev); err != nil {
		s.c.Logf("lawpay-docuseal: event %s (%s) failed: %v", ev.ID, ev.Action, err)
		_, _ = s.c.DB.ExecContext(context.WithoutCancel(r.Context()), `DELETE FROM received_events WHERE event_id = $1`, ev.ID)
		http.Error(w, "not handled", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleEvent(ctx context.Context, ev Event) error {
	switch ev.Action {
	case "payment.processor_authorized":
		return s.storeGrant(ctx, "payment", ev.ProcessorAuthorization, ev)
	case "payment.refund_authorized":
		return s.storeGrant(ctx, "refund", ev.RefundAuthorization, ev)
	case "file.background_authorized":
		g, ok := parseGrant(ev.BackgroundFileAuthorization, ev.TaskAuthorizationFromHeader)
		if !ok {
			return nil
		}
		matter := g.MatterID
		if matter == "" {
			matter = ev.MatterID
		}
		s.mu.Lock()
		s.fileGrants[matter] = g.Credential
		s.mu.Unlock()
		s.grants.notify("file:" + matter)
		return nil
	case "invoice.sent":
		return s.invoiceSent(ctx, ev.RecordID)
	case "invoice.updated", "payment.completed", "payment.refunded":
		return s.invoiceChanged(ctx, ev)
	case "invoice.voided":
		_, err := s.c.DB.ExecContext(ctx, `UPDATE payable_invoices SET status = 'void', closed_at = $2 WHERE invoice_id = $1`, ev.RecordID, s.now())
		return err
	case "trust.replenishment_requested":
		return s.depositsOn(ctx, ev.MatterID, "")
	case "conflict_check.decided":
		return s.conflictDecided(ctx, ev)
	}
	return nil
}

// storeGrant keeps a delivered processor or refund grant and wakes the request waiting
// for it.
func (s *Server) storeGrant(ctx context.Context, kind string, raw json.RawMessage, ev Event) error {
	g, ok := parseGrant(raw, ev.TaskAuthorizationFromHeader)
	if !ok {
		return nil
	}
	key := g.InitializationID
	if kind == "refund" {
		if g.RefundID == "" {
			g.RefundID = ev.RecordID
		}
		key = "refund:" + g.RefundID
	} else if key == "" {
		key = ev.RecordID
	}
	if g.PaymentID == "" && kind == "refund" {
		g.PaymentID = ev.RecordID
	}
	exp, err := time.Parse(time.RFC3339, g.ExpiresAt)
	if err != nil {
		exp = s.now().AddDate(0, 0, 1)
	}
	_, err = s.c.DB.ExecContext(ctx, `INSERT INTO processor_grants (initialization_id, kind, payment_id, refund_id, grant_credential, expires_at, received_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (initialization_id) DO UPDATE SET grant_credential = $5, expires_at = $6, received_at = $7`,
		key, kind, g.PaymentID, nullable(g.RefundID), g.Credential, exp, s.now())
	if err != nil {
		return err
	}
	if kind == "refund" {
		s.grants.notify("refund:" + g.RefundID)
	} else {
		s.grants.notify("proc:" + key)
	}
	return nil
}

// grantFor returns the stored grant credential under key.
func (s *Server) grantFor(ctx context.Context, key string) (string, error) {
	var cred string
	err := s.c.DB.QueryRowContext(ctx, `SELECT grant_credential FROM processor_grants WHERE initialization_id = $1`, key).Scan(&cred)
	if errors.Is(err, errNoRows) {
		return "", nil
	}
	return cred, err
}
