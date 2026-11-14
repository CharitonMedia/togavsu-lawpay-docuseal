package lawpaydocuseal

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"

	"github.com/CharitonMedia/togavsu-lawpay-docuseal/internal/coreapi"
)

// RefundRequest is a firm refund of a LawPay payment.
type RefundRequest struct {
	PaymentID   string `json:"payment_id"`
	AmountCents int64  `json:"amount_cents"`
	Reason      string `json:"reason"`
}

// RefundResult is what the refunding attorney sees.
type RefundResult struct {
	RefundID       string `json:"refund_id"`
	State          string `json:"state"`
	CoreRefundID   string `json:"core_refund_id,omitempty"`
	LawPayRefundID string `json:"lawpay_refund_id,omitempty"`
	Message        string `json:"message"`
	Warning        string `json:"warning,omitempty"`
}

var errExceeds = &appError{Status: http.StatusUnprocessableEntity, Code: "refund_exceeds_payment",
	Message: "That is more than is left to refund on this payment."}

// refundable reads the payment and its LawPay charge and returns what is left to refund:
// the payment amount less the core's completed refunds and the module's refunds in flight.
func (s *Server) refundable(ctx context.Context, a coreapi.Auth, paymentID string) (coreapi.Payment, charge, int64, error) {
	p, err := s.core.GetPayment(ctx, a, paymentID)
	if err != nil {
		if coreapi.Is(err, http.StatusNotFound) {
			return p, charge{}, 0, errNotFound
		}
		return p, charge{}, 0, err
	}
	c, found, err := s.chargeBy(ctx, "payment_id", p.ID)
	if err != nil {
		return p, c, 0, err
	}
	if !found || c.LawPayChargeID == "" {
		return p, c, 0, fail(http.StatusUnprocessableEntity, "not_lawpay", "This payment wasn't made through LawPay. Refund it from the core payment screen.")
	}
	var inFlight int64
	if err := s.c.DB.QueryRowContext(ctx, `SELECT COALESCE(sum(amount_cents), 0) FROM lawpay_refunds
		WHERE payment_id = $1 AND state IN ('core_pending', 'processor_sent', 'processor_done')`, p.ID).Scan(&inFlight); err != nil {
		return p, c, 0, err
	}
	return p, c, p.AmountCents - p.RefundedCents - inFlight, nil
}

// TrustWarning is the below-zero warning for a trust refund, or "" when the refund keeps
// the client's matter balance at or above zero. It never blocks the refund.
func (s *Server) TrustWarning(ctx context.Context, v viewer, p coreapi.Payment, amount int64) string {
	if p.Destination != "trust" {
		return ""
	}
	acct := ""
	if p.TrustAccountID != nil {
		acct = *p.TrustAccountID
	}
	b, err := s.core.TrustBalances(ctx, v.auth(), p.MatterID, p.ContactID, acct)
	if err != nil {
		return ""
	}
	after := b.MatterCents - amount
	if after >= 0 {
		return ""
	}
	return fmt.Sprintf("This leaves the client at %s in trust. It will post and raise a trust flag.", dollars(after))
}

// Refund runs a firm refund: core refund pending, LawPay refund, core refund completed.
// An amount over what is left is refused before any write or adapter call.
func (s *Server) Refund(ctx context.Context, v viewer, req RefundRequest) (RefundResult, error) {
	st, err := s.settings(ctx)
	if err != nil {
		return RefundResult{}, err
	}
	if !st.LawPayReady() {
		return RefundResult{}, errNoLawPay
	}
	if !v.staff() {
		return RefundResult{}, errNotFound
	}
	p, c, left, err := s.refundable(ctx, v.auth(), req.PaymentID)
	if err != nil {
		return RefundResult{}, err
	}
	if req.AmountCents <= 0 || req.AmountCents > left {
		return RefundResult{}, errExceeds
	}
	return s.runRefund(ctx, v, st, p, c, "", req.AmountCents, req.Reason, "firm")
}

// runRefund does the three steps. rowID names an existing proposed row, else a new row is
// made once the core refund exists.
func (s *Server) runRefund(ctx context.Context, v viewer, st Settings, p coreapi.Payment, c charge, rowID string, amount int64, reason, origin string) (RefundResult, error) {
	if rowID == "" {
		rowID = s.id()
	}
	a := v.auth()
	a.Idempotency = "lpds-refund-" + rowID
	if reason == "" {
		reason = "Refund of LawPay payment " + c.LawPayChargeID
	}
	warning := s.TrustWarning(ctx, v, p, amount)
	cr, err := s.core.CreateRefund(ctx, a, p.ID, map[string]any{"amount_cents": amount, "reason": reason, "status": "pending"})
	if err != nil {
		if coreapi.HasCode(err, "refund_exceeds_payment") {
			return RefundResult{}, errExceeds
		}
		return RefundResult{}, err
	}
	bg := context.WithoutCancel(ctx)
	res, err := s.c.DB.ExecContext(bg, `UPDATE lawpay_refunds SET core_refund_id = $2, state = 'core_pending', amount_cents = $3, reason = $4,
		requested_by = $5, updated_at = $6 WHERE id = $1 AND state = 'proposed'`, rowID, cr.ID, amount, reason, v.UserID, s.now())
	if err != nil {
		return RefundResult{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := s.c.DB.ExecContext(bg, `INSERT INTO lawpay_refunds (id, charge_id, payment_id, core_refund_id, amount_cents, reason, origin,
			state, requested_by, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, 'core_pending', $8, $9, $9)`,
			rowID, c.ID, p.ID, cr.ID, amount, reason, origin, v.UserID, s.now()); err != nil {
			return RefundResult{}, err
		}
	}
	out := RefundResult{RefundID: rowID, CoreRefundID: cr.ID, Warning: warning}
	if err := s.setRefund(bg, rowID, `state = 'processor_sent'`); err != nil {
		return out, err
	}
	tx, err := s.lawpay(st).Refund(bg, c.LawPayChargeID, amount)
	if err != nil {
		msg := lawpayMessage(err)
		if _, e := s.core.RefundStatus(bg, v.auth(), p.ID, cr.ID, "failed", nil); e != nil {
			s.c.Logf("lawpay-docuseal: core refund %s not marked failed: %v", cr.ID, e)
		}
		_ = s.setRefund(bg, rowID, `state = 'failed', failure_message = $3`, msg)
		out.State, out.Message = "failed", msg
		return out, &appError{Status: http.StatusBadGateway, Code: "refund_failed", Message: msg, Extra: map[string]any{"refund_id": rowID}}
	}
	out.LawPayRefundID = tx.ID
	if err := s.setRefund(bg, rowID, `state = 'processor_done', lawpay_refund_id = $3`, tx.ID); err != nil {
		return out, err
	}
	if _, err := s.core.RefundStatus(bg, v.auth(), p.ID, cr.ID, "completed", map[string]any{"reference_number": tx.ID}); err != nil {
		s.c.Logf("lawpay-docuseal: refund %s done in LawPay (%s), core completion deferred: %v", rowID, tx.ID, err)
		out.State, out.Message = "processor_done", "LawPay refunded the payment. Togavsu will post it shortly."
		return out, nil
	}
	_ = s.setRefund(bg, rowID, `state = 'posted'`)
	out.State, out.Message = "posted", "Refund posted."
	return out, nil
}

func (s *Server) setRefund(ctx context.Context, id, set string, args ...any) error {
	_, err := s.c.DB.ExecContext(ctx, `UPDATE lawpay_refunds SET `+set+`, updated_at = $2 WHERE id = $1`, append([]any{id, s.now()}, args...)...)
	return err
}

type refundRow struct {
	ID, ChargeID, PaymentID, CoreRefundID, LawPayRefundID, Origin, State, Reason string
	AmountCents                                                                  int64
	Attempts                                                                     int
}

func (s *Server) refundRow(ctx context.Context, col, val string) (refundRow, bool, error) {
	var r refundRow
	err := s.c.DB.QueryRowContext(ctx, `SELECT id, charge_id, payment_id, COALESCE(core_refund_id, ''), COALESCE(lawpay_refund_id, ''), origin, state,
		reason, amount_cents, attempts FROM lawpay_refunds WHERE `+col+` = $1`, val).
		Scan(&r.ID, &r.ChargeID, &r.PaymentID, &r.CoreRefundID, &r.LawPayRefundID, &r.Origin, &r.State, &r.Reason, &r.AmountCents, &r.Attempts)
	if errors.Is(err, errNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

// IssueProposed issues a proposed (no-go) refund with the firm refund sequence.
func (s *Server) IssueProposed(ctx context.Context, v viewer, id string) (RefundResult, error) {
	st, err := s.settings(ctx)
	if err != nil {
		return RefundResult{}, err
	}
	if !st.LawPayReady() {
		return RefundResult{}, errNoLawPay
	}
	r, found, err := s.refundRow(ctx, "id", id)
	if err != nil {
		return RefundResult{}, err
	}
	if !found || r.State != "proposed" {
		return RefundResult{}, errNotFound
	}
	p, c, left, err := s.refundable(ctx, v.auth(), r.PaymentID)
	if err != nil {
		return RefundResult{}, err
	}
	if r.AmountCents > left {
		return RefundResult{}, errExceeds
	}
	return s.runRefund(ctx, v, st, p, c, r.ID, r.AmountCents, r.Reason, r.Origin)
}

// DismissProposed drops a proposed refund.
func (s *Server) DismissProposed(ctx context.Context, v viewer, id string) error {
	if !v.canRefund() {
		return errForbidden
	}
	res, err := s.c.DB.ExecContext(ctx, `UPDATE lawpay_refunds SET state = 'dismissed', updated_at = $2 WHERE id = $1 AND state = 'proposed'`, id, s.now())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound
	}
	return nil
}

// conflictDecided proposes the consult fee refund when a conflict check ends in no go:
// the full remaining amount of the matter's latest completed payment, when that was a
// LawPay card charge, else a manual_refund_needed exception.
func (s *Server) conflictDecided(ctx context.Context, ev Event) error {
	cc, err := s.core.GetConflictCheck(ctx, coreapi.Service, ev.RecordID)
	if err != nil {
		if coreapi.Is(err, http.StatusNotFound) {
			return nil
		}
		return err
	}
	if !cc.NoGo() {
		return nil
	}
	matter := cc.MatterID
	if matter == "" {
		matter = ev.MatterID
	}
	if matter == "" {
		return nil
	}
	ps, err := s.core.Payments(ctx, coreapi.Service, url.Values{"matter_id": {matter}, "status": {"completed"}})
	if err != nil {
		return err
	}
	if len(ps) == 0 {
		return nil
	}
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].CreatedAt > ps[j].CreatedAt })
	p := ps[0]
	c, found, err := s.chargeBy(ctx, "payment_id", p.ID)
	if err != nil {
		return err
	}
	left := p.AmountCents - p.RefundedCents
	if p.Source != ID || !found || c.Method != "card" || left <= 0 {
		s.openException(ctx, exception{Kind: "manual_refund_needed", MatterID: matter, TransactionID: "payment:" + p.ID, AmountCents: left,
			Detail: "The conflict check ended in no go. The matter's latest payment wasn't a LawPay card charge, so refund it by hand."})
		return nil
	}
	var open int
	if err := s.c.DB.QueryRowContext(ctx, `SELECT count(*) FROM lawpay_refunds WHERE payment_id = $1 AND origin = 'no_go' AND state <> 'dismissed'`, p.ID).Scan(&open); err != nil {
		return err
	}
	if open > 0 {
		return nil
	}
	id := s.id()
	res, err := s.c.DB.ExecContext(ctx, `INSERT INTO lawpay_refunds (id, charge_id, payment_id, amount_cents, reason, origin, state, source_event_id,
		created_at, updated_at) VALUES ($1, $2, $3, $4, $5, 'no_go', 'proposed', $6, $7, $7) ON CONFLICT (source_event_id) DO NOTHING`,
		id, c.ID, p.ID, left, "Consult fee refund: the conflict check ended in no go", ev.ID, s.now())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	if err := s.core.Notify(ctx, coreapi.Service, "refund_proposed", "A consult fee refund is ready to issue",
		"/modules/"+ID+"/matter?matter_id="+url.QueryEscape(matter), "responsible_attorney", matter); err != nil {
		s.c.Logf("lawpay-docuseal: refund_proposed notification failed: %v", err)
	}
	return nil
}

// completeRefund posts a refund LawPay already made, with the refund grant the core
// delivered when the refund was created pending.
func (s *Server) completeRefund(ctx context.Context, r refundRow) error {
	cred, err := s.grantFor(ctx, "refund:"+r.CoreRefundID)
	if err != nil {
		return err
	}
	if cred == "" {
		return errors.New("lawpay-docuseal: no refund grant yet")
	}
	if _, err := s.core.RefundStatus(ctx, coreapi.Auth{Task: cred}, r.PaymentID, r.CoreRefundID, "completed",
		map[string]any{"reference_number": r.LawPayRefundID}); err != nil {
		return err
	}
	return s.setRefund(ctx, r.ID, `state = 'posted'`)
}

// PostExternal posts a verified dashboard refund as a completed core refund with the
// LawPay refund id as its reference, and resolves its exception.
func (s *Server) PostExternal(ctx context.Context, v viewer, exceptionID, note string) (RefundResult, error) {
	if !v.canRefund() {
		return RefundResult{}, errForbidden
	}
	var refundID string
	err := s.c.DB.QueryRowContext(ctx, `SELECT COALESCE(refund_id, '') FROM exceptions WHERE id = $1 AND kind = 'external_refund' AND status = 'open'`, exceptionID).Scan(&refundID)
	if errors.Is(err, errNoRows) || refundID == "" {
		return RefundResult{}, errNotFound
	}
	if err != nil {
		return RefundResult{}, err
	}
	r, _, err := s.refundRow(ctx, "id", refundID)
	if err != nil {
		return RefundResult{}, err
	}
	if r.State != "external_detected" {
		return RefundResult{}, errNotFound
	}
	a := v.auth()
	a.Idempotency = "lpds-refund-" + r.ID
	cr, err := s.core.CreateRefund(ctx, a, r.PaymentID, map[string]any{"amount_cents": r.AmountCents, "status": "completed",
		"reason": "Refund made in the LawPay dashboard", "reference_number": r.LawPayRefundID})
	if err != nil {
		return RefundResult{}, err
	}
	if err := s.setRefund(ctx, r.ID, `state = 'posted', core_refund_id = $3, requested_by = $4`, cr.ID, v.UserID); err != nil {
		return RefundResult{}, err
	}
	if note == "" {
		note = "Posted the LawPay dashboard refund."
	}
	if err := s.resolve(ctx, exceptionID, v.UserID, note); err != nil {
		return RefundResult{}, err
	}
	return RefundResult{RefundID: r.ID, State: "posted", CoreRefundID: cr.ID, LawPayRefundID: r.LawPayRefundID, Message: "Refund posted."}, nil
}

// resolve closes an open exception with a note.
func (s *Server) resolve(ctx context.Context, id, by, note string) error {
	res, err := s.c.DB.ExecContext(ctx, `UPDATE exceptions SET status = 'resolved', resolved_by = $2, resolved_at = $3, resolution_note = $4
		WHERE id = $1 AND status = 'open'`, id, by, s.now(), note)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound
	}
	return nil
}
