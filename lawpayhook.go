package lawpaydocuseal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/CharitonMedia/togavsu-lawpay-docuseal/adapters"
)

// malformedKeep is how much of an unparsable body a receipt keeps.
const malformedKeep = 4000

// Candidates are the fields a LawPay webhook body may carry, pulled from the places
// Unified looks (U7). They only say which transaction to read: every decision uses the
// live transaction, never the body.
type Candidates struct {
	TransactionID, Type, Status, Event, ChargeID string
}

func str(m map[string]any, path ...string) string {
	var cur any = m
	for _, p := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = obj[p]
	}
	switch v := cur.(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	case float64:
		return strings.TrimSuffix(strings.TrimRight(jsonNum(v), "0"), ".")
	}
	return ""
}

func jsonNum(v float64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func first(m map[string]any, paths ...[]string) string {
	for _, p := range paths {
		if v := str(m, p...); v != "" {
			return v
		}
	}
	return ""
}

// ExtractCandidates reads the candidate fields from a parsed body.
func ExtractCandidates(m map[string]any) Candidates {
	return Candidates{
		TransactionID: first(m, []string{"data", "id"}, []string{"data", "transaction", "id"}, []string{"transaction", "id"},
			[]string{"transaction_id"}, []string{"data", "transaction_id"}, []string{"id"}),
		Type:     strings.ToUpper(first(m, []string{"data", "type"}, []string{"data", "transaction", "type"}, []string{"transaction", "type"}, []string{"type"})),
		Status:   strings.ToUpper(first(m, []string{"data", "status"}, []string{"data", "transaction", "status"}, []string{"transaction", "status"}, []string{"status"})),
		Event:    first(m, []string{"event"}, []string{"event_type"}, []string{"type"}),
		ChargeID: first(m, []string{"data", "charge_id"}, []string{"data", "transaction", "charge_id"}, []string{"transaction", "charge_id"}, []string{"charge_id"}),
	}
}

// LooksLikeRefund is the refund-first check (U8): a body that names a refund in its type
// or event, or carries a charge_id, is handled as a refund and never completes the charge
// it names.
func (c Candidates) LooksLikeRefund() bool {
	return c.Type == "REFUND" || strings.Contains(strings.ToLower(c.Event), "refund") || c.ChargeID != ""
}

// DedupeKey is the receipt key: the SHA-256 of the exact body bytes.
func DedupeKey(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// lawpayHook receives LawPay webhooks through the core's inbound hook route. A wrong or
// missing token writes nothing. Everything else is stored first and answers 200.
func (s *Server) lawpayHook(w http.ResponseWriter, r *http.Request) {
	st, err := s.settings(r.Context())
	if err != nil {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	if !tokenEqual(r.URL.Query().Get("token"), st.LawPayHookToken) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "unauthenticated"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	key := DedupeKey(body)
	var parsed map[string]any
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	payload, verdict := body, "received"
	if err := dec.Decode(&parsed); err != nil || parsed == nil {
		raw := body
		if len(raw) > malformedKeep {
			raw = raw[:malformedKeep]
		}
		text := strings.ToValidUTF8(string(raw), string(utf8.RuneError))
		payload, _ = json.Marshal(map[string]string{"raw": text})
		verdict = "malformed"
	}
	cand := ExtractCandidates(parsed)
	id := s.id()
	res, err := s.c.DB.ExecContext(ctx, `INSERT INTO lawpay_webhook_receipts (id, received_at, dedupe_key, transaction_id, event_type, payload, verdict)
		VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (dedupe_key) DO NOTHING`, id, s.now(), key, cand.TransactionID, cand.Event, string(payload), verdict)
	if err != nil {
		http.Error(w, "not stored", http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		_, _ = s.c.DB.ExecContext(ctx, `UPDATE lawpay_webhook_receipts SET duplicate_count = duplicate_count + 1 WHERE dedupe_key = $1`, key)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "verdict": "duplicate"})
		return
	}
	if verdict == "malformed" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "verdict": verdict})
		return
	}
	verdict, err = s.processLawPay(context.WithoutCancel(ctx), st, id, cand)
	if err != nil {
		// Kept as received: the sweep verifies it again.
		s.c.Logf("lawpay-docuseal: LawPay receipt %s kept for the sweep: %v", id, err)
		verdict = "received"
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "verdict": verdict})
}

func (s *Server) setReceipt(ctx context.Context, id, verdict, chargeID, refundID string) error {
	_, err := s.c.DB.ExecContext(ctx, `UPDATE lawpay_webhook_receipts SET verdict = $2, charge_id = $3, refund_id = $4 WHERE id = $1`,
		id, verdict, nullable(chargeID), nullable(refundID))
	return err
}

// processLawPay verifies a receipt's transaction with a live read and applies it. An
// error leaves the receipt received for the sweep.
func (s *Server) processLawPay(ctx context.Context, st Settings, receiptID string, cand Candidates) (string, error) {
	if cand.TransactionID == "" {
		return "ignored", s.setReceipt(ctx, receiptID, "ignored", "", "")
	}
	tx, err := s.lawpay(st).Transaction(ctx, cand.TransactionID)
	if err != nil {
		if adapters.NotFound(err) {
			return "unverified", s.setReceipt(ctx, receiptID, "unverified", "", "")
		}
		return "", err
	}
	if strings.EqualFold(tx.Type, "REFUND") || cand.LooksLikeRefund() {
		if !strings.EqualFold(tx.Type, "REFUND") {
			// The body says refund but LawPay says this id is a charge: fail closed.
			return "ignored", s.setReceipt(ctx, receiptID, "ignored", "", "")
		}
		return s.applyRefund(ctx, st, receiptID, tx)
	}
	return s.applyCharge(ctx, st, receiptID, tx)
}

func (s *Server) applyRefund(ctx context.Context, st Settings, receiptID string, tx adapters.Transaction) (string, error) {
	if r, found, err := s.refundRow(ctx, "lawpay_refund_id", tx.ID); err != nil {
		return "", err
	} else if found {
		// A refund the module made: the stored LawPay refund id matches, nothing to do here
		// (a processor_done row is posted by the sweep with its grant).
		return "applied", s.setReceipt(ctx, receiptID, "applied", r.ChargeID, r.ID)
	}
	c, found, err := s.chargeBy(ctx, "lawpay_charge_id", tx.ChargeID)
	if err != nil {
		return "", err
	}
	if !found || c.PaymentID == "" {
		s.openException(ctx, exception{Kind: "unknown_transaction", TransactionID: tx.ID, AmountCents: tx.AmountCents,
			Detail: "LawPay shows a refund of a charge Togavsu has no record of."})
		return "unknown_transaction", s.setReceipt(ctx, receiptID, "unknown_transaction", "", "")
	}
	if adapters.Map(tx, false) != adapters.OutcomeRefundDone {
		return "ignored", s.setReceipt(ctx, receiptID, "ignored", c.ID, "")
	}
	id := s.id()
	if _, err := s.c.DB.ExecContext(ctx, `INSERT INTO lawpay_refunds (id, charge_id, payment_id, lawpay_refund_id, amount_cents, reason, origin, state,
		created_at, updated_at) VALUES ($1, $2, $3, $4, $5, 'Refund made in the LawPay dashboard', 'lawpay_dashboard', 'external_detected', $6, $6)
		ON CONFLICT (lawpay_refund_id) DO NOTHING`, id, c.ID, c.PaymentID, tx.ID, tx.AmountCents, s.now()); err != nil {
		return "", err
	}
	r, _, err := s.refundRow(ctx, "lawpay_refund_id", tx.ID)
	if err != nil {
		return "", err
	}
	s.openException(ctx, exception{Kind: "external_refund", MatterID: c.MatterID, ChargeID: c.ID, RefundID: r.ID, TransactionID: tx.ID,
		AmountCents: tx.AmountCents, Detail: "A refund of " + dollars(tx.AmountCents) + " was made in the LawPay dashboard. Post it to record it in Togavsu."})
	return "applied", s.setReceipt(ctx, receiptID, "applied", c.ID, r.ID)
}

func (s *Server) applyCharge(ctx context.Context, st Settings, receiptID string, tx adapters.Transaction) (string, error) {
	c, found, err := s.chargeBy(ctx, "lawpay_charge_id", tx.ID)
	if err != nil {
		return "", err
	}
	if !found {
		if tx.AccountID != st.OperatingAccount && tx.AccountID != st.TrustAccount {
			s.openException(ctx, exception{Kind: "account_mismatch", TransactionID: tx.ID, AmountCents: tx.AmountCents,
				Detail: "LawPay sent a charge on an account that matches neither LawPay account in the settings."})
			return "ignored", s.setReceipt(ctx, receiptID, "ignored", "", "")
		}
		s.openException(ctx, exception{Kind: "unknown_transaction", TransactionID: tx.ID, AmountCents: tx.AmountCents,
			Detail: "LawPay shows a charge of " + dollars(tx.AmountCents) + " that Togavsu has no record of. Pick the matter and contact to record it."})
		return "unknown_transaction", s.setReceipt(ctx, receiptID, "unknown_transaction", "", "")
	}
	if tx.AccountID != st.account(c.Destination) {
		s.openException(ctx, exception{Kind: "account_mismatch", MatterID: c.MatterID, ChargeID: c.ID, TransactionID: tx.ID, AmountCents: tx.AmountCents,
			Detail: "LawPay shows this charge on a different account than its destination. Nothing was posted."})
		return "ignored", s.setReceipt(ctx, receiptID, "ignored", c.ID, "")
	}
	if tx.AmountCents != c.AmountCents {
		s.openException(ctx, exception{Kind: "amount_mismatch", MatterID: c.MatterID, ChargeID: c.ID, TransactionID: tx.ID, AmountCents: tx.AmountCents,
			Detail: "LawPay shows " + dollars(tx.AmountCents) + " for a charge of " + dollars(c.AmountCents) + ". Nothing was posted."})
		return "ignored", s.setReceipt(ctx, receiptID, "ignored", c.ID, "")
	}
	if c.State == "recorded_pending" {
		if out := adapters.Map(tx, true); out == adapters.OutcomeCompleted || out == adapters.OutcomeFailed {
			if err := s.settle(ctx, c, st, out, tx.Status); err != nil {
				return "", err
			}
		}
	}
	return "applied", s.setReceipt(ctx, receiptID, "applied", c.ID, "")
}

// RecordUnknown records a verified unknown LawPay charge as a core payment on the matter
// and contact an attorney picked: the verified amount and method, and the destination the
// account id names. The exception resolves.
func (s *Server) RecordUnknown(ctx context.Context, v viewer, exceptionID, matter, contact string) (ChargeResult, error) {
	if !v.canRefund() {
		return ChargeResult{}, errForbidden
	}
	st, err := s.settings(ctx)
	if err != nil {
		return ChargeResult{}, err
	}
	if !st.LawPayReady() {
		return ChargeResult{}, errNoLawPay
	}
	var txID string
	err = s.c.DB.QueryRowContext(ctx, `SELECT transaction_id FROM exceptions WHERE id = $1 AND kind = 'unknown_transaction' AND status = 'open'`, exceptionID).Scan(&txID)
	if errors.Is(err, errNoRows) {
		return ChargeResult{}, errNotFound
	}
	if err != nil {
		return ChargeResult{}, err
	}
	tx, err := s.lawpay(st).Transaction(ctx, txID)
	if err != nil {
		return ChargeResult{}, err
	}
	dest := ""
	switch tx.AccountID {
	case st.OperatingAccount:
		dest = "operating"
	case st.TrustAccount:
		dest = "trust"
	default:
		return ChargeResult{}, fail(http.StatusUnprocessableEntity, "account_mismatch", "The LawPay account matches neither account in the settings.")
	}
	if strings.EqualFold(tx.Type, "REFUND") {
		return ChargeResult{}, fail(http.StatusUnprocessableEntity, "validation_failed", "A refund can't be recorded as a payment.")
	}
	out := adapters.Map(tx, false)
	status := "completed"
	switch out {
	case adapters.OutcomePending:
		status = "pending"
	case adapters.OutcomeCompleted:
	default:
		return ChargeResult{}, fail(http.StatusUnprocessableEntity, "validation_failed", "LawPay shows this charge didn't go through.")
	}
	id := s.id()
	method := adapters.Method(tx.MethodType)
	body := map[string]any{"contact_id": contact, "matter_id": matter, "amount_cents": tx.AmountCents, "destination": dest, "method": method,
		"reference_number": tx.ID, "status": status, "source": ID}
	if dest == "trust" {
		body["trust_account_id"] = st.CoreTrustAccount
	}
	a := v.auth()
	a.Idempotency = "lpds-unknown-" + tx.ID
	p, err := s.core.CreatePayment(ctx, a, body)
	if err != nil {
		return ChargeResult{}, err
	}
	state := "recorded_completed"
	if p.Status == "pending" {
		state = "recorded_pending"
	}
	if _, err := s.c.DB.ExecContext(ctx, `INSERT INTO lawpay_charges (id, matter_id, contact_id, payment_id, purpose, channel, destination, method,
		amount_cents, lawpay_charge_id, lawpay_status, state, initiated_by, expires_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'staff_charge', 'staff', $5, $6, $7, $8, $9, $10, $11, $12, $12, $12)`,
		id, matter, contact, p.ID, dest, method, tx.AmountCents, tx.ID, tx.Status, state, v.UserID, s.now()); err != nil {
		return ChargeResult{}, err
	}
	if err := s.resolve(ctx, exceptionID, v.UserID, "Recorded as a payment on the matter."); err != nil {
		return ChargeResult{}, err
	}
	return ChargeResult{ChargeID: id, State: state, LawPayChargeID: tx.ID, PaymentID: p.ID, Message: "Recorded."}, nil
}
