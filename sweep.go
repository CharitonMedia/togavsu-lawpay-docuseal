package lawpaydocuseal

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/CharitonMedia/togavsu-lawpay-docuseal/adapters"
)

// Run sweeps every reconcile_interval_minutes until ctx ends. The interval is read again
// before each wait, so a settings change takes effect on the next sweep.
func (s *Server) Run(ctx context.Context) {
	for {
		st, err := s.Sweep(ctx)
		if err != nil {
			s.c.Logf("lawpay-docuseal: sweep: %v", err)
		}
		wait := time.Duration(st.ReconcileMinutes) * time.Minute
		if wait <= 0 {
			wait = time.Minute
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// Sweep is one reconciliation pass: refresh readiness, expire old intents, retry
// unrecorded charges, advance pending ACH charges, post refunds LawPay already made,
// re-verify webhook receipts left received, and retry signed PDFs.
func (s *Server) Sweep(ctx context.Context) (Settings, error) {
	st, err := s.RefreshState(ctx)
	if err != nil {
		return st, err
	}
	var errs []string
	note := func(what string, err error) {
		if err != nil {
			errs = append(errs, what+": "+err.Error())
		}
	}
	_, err = s.c.DB.ExecContext(ctx, `UPDATE lawpay_charges SET state = 'expired', failure_code = $2, updated_at = $1
		WHERE state = 'intent' AND expires_at <= $1`, s.now(), codeExpired)
	note("expire intents", err)
	if st.LawPayReady() {
		note("unrecorded charges", s.sweepUnrecorded(ctx, st))
		note("pending ACH", s.sweepPending(ctx, st))
		note("refunds", s.sweepRefunds(ctx, st))
		note("receipts", s.sweepReceipts(ctx, st))
	}
	if st.DocuSealReady() {
		note("signed PDFs", s.sweepPDFs(ctx, st))
	}
	if len(errs) > 0 {
		return st, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return st, nil
}

func (s *Server) charges(ctx context.Context, where string, args ...any) ([]charge, error) {
	rows, err := s.c.DB.QueryContext(ctx, `SELECT `+chargeCols+` FROM lawpay_charges WHERE `+where+` ORDER BY created_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []charge
	for rows.Next() {
		c, err := scanCharge(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// sweepUnrecorded retries createPayment for charged_unrecorded charges with the same
// idempotency key and grant. After retry_limit failed retries it opens one exception.
func (s *Server) sweepUnrecorded(ctx context.Context, st Settings) error {
	cs, err := s.charges(ctx, `state = 'charged_unrecorded'`)
	if err != nil {
		return err
	}
	for _, c := range cs {
		cred, err := s.grantFor(ctx, c.InitializationID)
		if err != nil {
			return err
		}
		status := "completed"
		if adapters.Map(adapters.Transaction{Type: "CHARGE", Status: c.LawPayStatus, MethodType: methodType(c.Method)}, false) == adapters.OutcomePending {
			status = "pending"
		}
		attempts := c.Attempts
		res, err := s.record(ctx, c, st, cred, status)
		if err != nil {
			return err
		}
		if res.State != "charged_unrecorded" {
			continue
		}
		attempts++
		if err := s.setCharge(ctx, c.ID, `attempts = $3`, attempts); err != nil {
			return err
		}
		if attempts-1 >= st.RetryLimit {
			s.openException(ctx, exception{Kind: "charged_unrecorded", MatterID: c.MatterID, ChargeID: c.ID, TransactionID: c.LawPayChargeID,
				AmountCents: c.AmountCents, Detail: "LawPay charged " + dollars(c.AmountCents) + " (transaction " + c.LawPayChargeID +
					") but Togavsu couldn't record the payment after the automatic retries."})
		}
	}
	return nil
}

func methodType(method string) string {
	if method == "ach" {
		return "bank"
	}
	return "card"
}

// sweepPending reads every pending ACH charge younger than pending_check_days and
// advances it; an older one opens pending_ach_stale.
func (s *Server) sweepPending(ctx context.Context, st Settings) error {
	cs, err := s.charges(ctx, `state = 'recorded_pending'`)
	if err != nil {
		return err
	}
	cutoff := s.now().Add(-time.Duration(st.PendingCheckDays) * 24 * time.Hour)
	for _, c := range cs {
		if c.CreatedAt.Before(cutoff) {
			s.openException(ctx, exception{Kind: "pending_ach_stale", MatterID: c.MatterID, ChargeID: c.ID, TransactionID: c.LawPayChargeID,
				AmountCents: c.AmountCents, Detail: "This bank payment is still pending in LawPay after the check window. Check it in LawPay."})
			continue
		}
		tx, err := s.lawpay(st).Transaction(ctx, c.LawPayChargeID)
		if err != nil {
			continue
		}
		if out := adapters.Map(tx, true); out == adapters.OutcomeCompleted || out == adapters.OutcomeFailed {
			if err := s.settle(ctx, c, st, out, tx.Status); err != nil {
				s.c.Logf("lawpay-docuseal: pending charge %s not settled: %v", c.ID, err)
			}
		}
	}
	return nil
}

// sweepRefunds posts refunds LawPay made whose core completion failed.
func (s *Server) sweepRefunds(ctx context.Context, st Settings) error {
	rows, err := s.c.DB.QueryContext(ctx, `SELECT id FROM lawpay_refunds WHERE state = 'processor_done' ORDER BY created_at`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		r, _, err := s.refundRow(ctx, "id", id)
		if err != nil {
			return err
		}
		if err := s.completeRefund(ctx, r); err == nil {
			continue
		}
		if err := s.setRefund(ctx, r.ID, `attempts = attempts + 1`); err != nil {
			return err
		}
		if r.Attempts+1 >= st.RetryLimit {
			c, _, _ := s.chargeBy(ctx, "id", r.ChargeID)
			s.openException(ctx, exception{Kind: "refund_unposted", MatterID: c.MatterID, ChargeID: r.ChargeID, RefundID: r.ID,
				TransactionID: r.LawPayRefundID, AmountCents: r.AmountCents,
				Detail: "LawPay refunded " + dollars(r.AmountCents) + " but Togavsu couldn't post the refund."})
		}
	}
	return nil
}

// sweepReceipts verifies receipts a LawPay outage left received.
func (s *Server) sweepReceipts(ctx context.Context, st Settings) error {
	rows, err := s.c.DB.QueryContext(ctx, `SELECT id, payload FROM lawpay_webhook_receipts WHERE verdict = 'received' ORDER BY received_at`)
	if err != nil {
		return err
	}
	type rec struct {
		id      string
		payload []byte
	}
	var recs []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.id, &r.payload); err != nil {
			rows.Close()
			return err
		}
		recs = append(recs, r)
	}
	rows.Close()
	for _, r := range recs {
		dec := json.NewDecoder(strings.NewReader(string(r.payload)))
		dec.UseNumber()
		var m map[string]any
		if dec.Decode(&m) != nil {
			continue
		}
		if _, err := s.processLawPay(ctx, st, r.id, ExtractCandidates(m)); err != nil {
			s.c.Logf("lawpay-docuseal: receipt %s still unverified: %v", r.id, err)
		}
	}
	return nil
}
