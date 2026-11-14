package lawpaydocuseal

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"time"
)

// Transaction is one row of the reconciliation tab: a LawPay charge or refund.
type Transaction struct {
	Kind           string    `json:"kind"`
	ID             string    `json:"id"`
	MatterID       string    `json:"matter_id"`
	AmountCents    int64     `json:"amount_cents"`
	Destination    string    `json:"destination"`
	Method         string    `json:"method,omitempty"`
	State          string    `json:"state"`
	LawPayID       string    `json:"lawpay_id,omitempty"`
	PaymentID      string    `json:"payment_id,omitempty"`
	PaymentLink    string    `json:"payment_link,omitempty"`
	FailureMessage string    `json:"failure_message,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// Exception is an open or resolved exception.
type Exception struct {
	ID             string     `json:"id"`
	Kind           string     `json:"kind"`
	Title          string     `json:"title"`
	MatterID       string     `json:"matter_id,omitempty"`
	ChargeID       string     `json:"charge_id,omitempty"`
	RefundID       string     `json:"refund_id,omitempty"`
	SigningID      string     `json:"signing_id,omitempty"`
	TransactionID  string     `json:"transaction_id,omitempty"`
	AmountCents    int64      `json:"amount_cents"`
	Detail         string     `json:"detail"`
	Status         string     `json:"status"`
	ResolutionNote string     `json:"resolution_note,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	ResolvedAt     *time.Time `json:"resolved_at,omitempty"`
}

// visibility caches the viewer's matter reads for one request.
type visibility struct {
	s    *Server
	v    viewer
	seen map[string]bool
}

func (s *Server) visible(v viewer) *visibility {
	return &visibility{s: s, v: v, seen: map[string]bool{}}
}

func (vis *visibility) ok(ctx context.Context, matter string) bool {
	if matter == "" {
		return vis.v.Has("admin", "attorney")
	}
	if ok, done := vis.seen[matter]; done {
		return ok
	}
	ok := vis.s.canSee(ctx, vis.v, matter)
	vis.seen[matter] = ok
	return ok
}

func paymentLink(id string) string {
	if id == "" {
		return ""
	}
	return "/money/payments/" + id
}

// Transactions lists LawPay charges and refunds the viewer can see, filtered by state and
// kind (charge or refund).
func (s *Server) Transactions(ctx context.Context, v viewer, state, kind string) ([]Transaction, error) {
	if !v.staff() {
		return nil, errNotFound
	}
	out := []Transaction{}
	vis := s.visible(v)
	if kind == "" || kind == "charge" {
		where, args := `state <> 'intent'`, []any{}
		if state != "" {
			where, args = `state = $1`, []any{state}
		}
		cs, err := s.charges(ctx, where, args...)
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			if !vis.ok(ctx, c.MatterID) {
				continue
			}
			out = append(out, Transaction{Kind: "charge", ID: c.ID, MatterID: c.MatterID, AmountCents: c.AmountCents, Destination: c.Destination,
				Method: c.Method, State: c.State, LawPayID: c.LawPayChargeID, PaymentID: c.PaymentID, PaymentLink: paymentLink(c.PaymentID),
				FailureMessage: c.FailureMessage, CreatedAt: c.CreatedAt.UTC()})
		}
	}
	if kind == "" || kind == "refund" {
		q := `SELECT r.id, c.matter_id, r.amount_cents, c.destination, COALESCE(c.method, ''), r.state, COALESCE(r.lawpay_refund_id, ''), r.payment_id,
			r.failure_message, r.created_at FROM lawpay_refunds r JOIN lawpay_charges c ON c.id = r.charge_id`
		var args []any
		if state != "" {
			q += ` WHERE r.state = $1`
			args = append(args, state)
		}
		rows, err := s.c.DB.QueryContext(ctx, q+` ORDER BY r.created_at`, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var t Transaction
			t.Kind = "refund"
			if err := rows.Scan(&t.ID, &t.MatterID, &t.AmountCents, &t.Destination, &t.Method, &t.State, &t.LawPayID, &t.PaymentID,
				&t.FailureMessage, &t.CreatedAt); err != nil {
				return nil, err
			}
			t.CreatedAt, t.PaymentLink = t.CreatedAt.UTC(), paymentLink(t.PaymentID)
			if vis.ok(ctx, t.MatterID) {
				out = append(out, t)
			}
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Exceptions lists exceptions by kind and status (open by default).
func (s *Server) Exceptions(ctx context.Context, v viewer, kind, status string) ([]Exception, error) {
	if !v.Has("admin", "attorney") {
		return nil, errForbidden
	}
	if status == "" {
		status = "open"
	}
	q := `SELECT id, kind, COALESCE(matter_id, ''), COALESCE(charge_id, ''), COALESCE(refund_id, ''), COALESCE(signing_id, ''), transaction_id,
		amount_cents, detail, status, resolution_note, created_at, resolved_at FROM exceptions WHERE status = $1`
	args := []any{status}
	if kind != "" {
		q += ` AND kind = $2`
		args = append(args, kind)
	}
	rows, err := s.c.DB.QueryContext(ctx, q+` ORDER BY created_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	vis := s.visible(v)
	out := []Exception{}
	for rows.Next() {
		var e Exception
		var resolved sql.NullTime
		if err := rows.Scan(&e.ID, &e.Kind, &e.MatterID, &e.ChargeID, &e.RefundID, &e.SigningID, &e.TransactionID, &e.AmountCents, &e.Detail,
			&e.Status, &e.ResolutionNote, &e.CreatedAt, &resolved); err != nil {
			return nil, err
		}
		e.Title, e.CreatedAt = exceptionTitle(e.Kind), e.CreatedAt.UTC()
		if resolved.Valid {
			t := resolved.Time.UTC()
			e.ResolvedAt = &t
		}
		if e.MatterID == "" || vis.ok(ctx, e.MatterID) {
			out = append(out, e)
		}
	}
	return out, rows.Err()
}

// Resolve closes an exception; the note is required.
func (s *Server) Resolve(ctx context.Context, v viewer, id, note string) error {
	if !v.Has("admin", "attorney") {
		return errForbidden
	}
	if strings.TrimSpace(note) == "" {
		return &appError{Status: http.StatusUnprocessableEntity, Code: "validation_failed", Message: "Add a note saying how it was resolved.", Fields: []string{"note"}}
	}
	return s.resolve(ctx, id, v.UserID, strings.TrimSpace(note))
}

// Reopen opens online payment again on an invoice funded through trust.
func (s *Server) Reopen(ctx context.Context, v viewer, invoiceID string) error {
	if !v.canCharge() {
		return errForbidden
	}
	var matter string
	if err := s.c.DB.QueryRowContext(ctx, `SELECT matter_id FROM payable_invoices WHERE invoice_id = $1`, invoiceID).Scan(&matter); err != nil {
		return errNotFound
	}
	if !s.canSee(ctx, v, matter) {
		return errNotFound
	}
	res, err := s.c.DB.ExecContext(ctx, `UPDATE payable_invoices SET status = 'open', funded_cents = 0, reopened_by = $2 WHERE invoice_id = $1 AND status = 'funded'`,
		invoiceID, v.UserID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == noRows {
		return fail(http.StatusConflict, "not_funded", "Online payment is already open on this invoice.")
	}
	return nil
}

// SetDeposits turns client trust deposits on a matter on or off.
func (s *Server) SetDeposits(ctx context.Context, v viewer, matter string, enabled bool) error {
	if !v.canCharge() {
		return errForbidden
	}
	if !s.canSee(ctx, v, matter) {
		return errNotFound
	}
	_, err := s.c.DB.ExecContext(ctx, `INSERT INTO matter_deposits (matter_id, deposits_enabled, updated_by, updated_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (matter_id) DO UPDATE SET deposits_enabled = $2, updated_by = $3, updated_at = $4`, matter, enabled, v.UserID, s.now())
	return err
}

// AwaitingCount counts agreements awaiting a signature (sent and client_signed) on
// matters the viewer can see, by state.
func (s *Server) AwaitingCount(ctx context.Context, v viewer) (map[string]int, error) {
	if !v.staff() {
		return nil, errNotFound
	}
	gs, err := s.signings(ctx, `state IN ('sent', 'client_signed')`)
	if err != nil {
		return nil, err
	}
	vis := s.visible(v)
	out := map[string]int{"sent": zero, "client_signed": zero}
	for _, g := range gs {
		if vis.ok(ctx, g.MatterID) {
			out[g.State]++
		}
	}
	return out, nil
}
