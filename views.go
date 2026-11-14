package lawpaydocuseal

import (
	"context"
	"database/sql"
	"errors"

	"github.com/CharitonMedia/togavsu-lawpay-docuseal/internal/coreapi"
)

var errNoRows = sql.ErrNoRows

// maxBody bounds a request body the module reads; the core's inbound limit
// (modules.inbound_webhook_max_bytes) is lower, so this only guards a direct caller.
const maxBody = 1 << 20

// invoiceSent opens the invoice's payable row, with the destination invoice_pay_destination
// names now, and the matter's deposit row.
func (s *Server) invoiceSent(ctx context.Context, invoiceID string) error {
	inv, err := s.core.GetInvoice(ctx, coreapi.Service, invoiceID)
	if err != nil {
		if coreapi.Is(err, 404) {
			return nil // walled or gone: the books reader gets what a user outside the wall gets
		}
		return err
	}
	st, err := s.settings(ctx)
	if err != nil {
		return err
	}
	dest := st.InvoiceDestination
	if dest != "operating" {
		dest = "trust"
	}
	status := "open"
	if inv.BalanceDueCents <= 0 {
		status = "paid"
	}
	if _, err := s.c.DB.ExecContext(ctx, `INSERT INTO payable_invoices (invoice_id, matter_id, destination, status, opened_at)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT (invoice_id) DO UPDATE SET status = CASE WHEN payable_invoices.status = 'void'
		THEN EXCLUDED.status ELSE payable_invoices.status END`, inv.ID, inv.MatterID, dest, status, s.now()); err != nil {
		return err
	}
	return s.depositsOn(ctx, inv.MatterID, "")
}

// invoiceChanged re-reads the balance of the invoice an event touches: a balance of zero
// is paid, and a balance that reappears (a refund) opens it again.
func (s *Server) invoiceChanged(ctx context.Context, ev Event) error {
	invoiceID := ev.RecordID
	if ev.RecordType == "payment" || ev.Action != "invoice.updated" {
		p, err := s.core.GetPayment(ctx, coreapi.Service, ev.RecordID)
		if err != nil {
			if coreapi.Is(err, 404) {
				return nil
			}
			return err
		}
		if p.InvoiceID == nil {
			return nil
		}
		invoiceID = *p.InvoiceID
	}
	return s.syncInvoice(ctx, invoiceID)
}

func (s *Server) syncInvoice(ctx context.Context, invoiceID string) error {
	var status string
	err := s.c.DB.QueryRowContext(ctx, `SELECT status FROM payable_invoices WHERE invoice_id = $1`, invoiceID).Scan(&status)
	if errors.Is(err, errNoRows) || status == "void" {
		return nil
	}
	if err != nil {
		return err
	}
	inv, err := s.core.GetInvoice(ctx, coreapi.Service, invoiceID)
	if err != nil {
		if coreapi.Is(err, 404) {
			return nil
		}
		return err
	}
	switch {
	case inv.Status == "void":
		_, err = s.c.DB.ExecContext(ctx, `UPDATE payable_invoices SET status = 'void', closed_at = $2 WHERE invoice_id = $1`, invoiceID, s.now())
	case inv.BalanceDueCents <= 0:
		_, err = s.c.DB.ExecContext(ctx, `UPDATE payable_invoices SET status = 'paid', closed_at = $2 WHERE invoice_id = $1`, invoiceID, s.now())
	case status == "paid":
		_, err = s.c.DB.ExecContext(ctx, `UPDATE payable_invoices SET status = 'open', closed_at = NULL, funded_cents = 0 WHERE invoice_id = $1`, invoiceID)
	}
	return err
}

// depositsOn makes sure the matter has a deposit row (on by default). It never turns a
// matter back on that staff turned off.
func (s *Server) depositsOn(ctx context.Context, matter, by string) error {
	if matter == "" {
		return nil
	}
	_, err := s.c.DB.ExecContext(ctx, `INSERT INTO matter_deposits (matter_id, deposits_enabled, updated_by, updated_at)
		VALUES ($1, true, $2, $3) ON CONFLICT (matter_id) DO NOTHING`, matter, nullable(by), s.now())
	return err
}

// payability is an invoice_payability row.
type payability struct {
	InvoiceID       string `json:"invoice_id"`
	Payable         bool   `json:"payable"`
	UnpayableReason string `json:"unpayable_reason"`
	InProgressCents int64  `json:"in_progress_cents"`
	Destination     string `json:"destination"`
	MatterID        string `json:"matter_id"`
}

func (s *Server) invoicePayability(ctx context.Context, invoiceID string) (payability, bool, error) {
	var p payability
	err := s.c.DB.QueryRowContext(ctx, `SELECT v.invoice_id, v.payable, v.unpayable_reason, v.in_progress_cents, p.destination, p.matter_id
		FROM invoice_payability v JOIN payable_invoices p USING (invoice_id) WHERE v.invoice_id = $1`, invoiceID).
		Scan(&p.InvoiceID, &p.Payable, &p.UnpayableReason, &p.InProgressCents, &p.Destination, &p.MatterID)
	if errors.Is(err, errNoRows) {
		return p, false, nil
	}
	return p, err == nil, err
}

func (s *Server) matterPayability(ctx context.Context, matter string) (enabled bool, minimum int64, found bool, err error) {
	err = s.c.DB.QueryRowContext(ctx, `SELECT trust_deposits_enabled, minimum_deposit_cents FROM matter_payability WHERE matter_id = $1`, matter).
		Scan(&enabled, &minimum)
	if errors.Is(err, errNoRows) {
		return false, 0, false, nil
	}
	return enabled, minimum, err == nil, err
}
