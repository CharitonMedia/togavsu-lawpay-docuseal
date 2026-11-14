package lawpaydocuseal

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/CharitonMedia/togavsu-lawpay-docuseal/adapters"
	"github.com/CharitonMedia/togavsu-lawpay-docuseal/internal/coreapi"
)

// Unified's PAY- failure codes, kept on the charge row.
const (
	codeDeclined   = "PAY-CHRG-DECLINED"
	codeError      = "PAY-CHRG-ERROR"
	codeUnrecorded = "PAY-CHRG-UNRECORDED"
	codeDenied     = "PAY-AUTH-DENIED"
	codeNoGrant    = "PAY-AUTH-TIMEOUT"
	codeExpired    = "PAY-INTENT-EXPIRED"
	codeFailed     = "PAY-ACH-FAILED"
)

// Messages the payer sees.
const (
	msgDeclined     = "Card declined"
	msgNotPayable   = "This invoice can't be paid online right now."
	msgUnrecorded   = "Your payment went through in LawPay (transaction %s), but we couldn't record it yet. Please contact the firm with that transaction id; you won't be charged again."
	msgIntentExpire = "This payment page expired. Start again."
	msgTooMany      = "Too many payment attempts. Wait a few minutes and try again."
	msgTrustOff     = "Trust payments aren't set up."
)

// PayRequest starts a pay intent. A client sets Purpose and the record ids (and the amount
// only for an open trust deposit); staff set the matter, amount, destination and,
// optionally, the contact and an operating invoice.
type PayRequest struct {
	Purpose                string `json:"purpose"`
	InvoiceID              string `json:"invoice_id"`
	MatterID               string `json:"matter_id"`
	ReplenishmentRequestID string `json:"replenishment_request_id"`
	ContactID              string `json:"contact_id"`
	Destination            string `json:"destination"`
	AmountCents            int64  `json:"amount_cents"`
}

// Intent is a pay intent: the server-decided amount and where it lands.
type Intent struct {
	ID          string    `json:"id"`
	Purpose     string    `json:"purpose"`
	MatterID    string    `json:"matter_id"`
	InvoiceID   string    `json:"invoice_id,omitempty"`
	Destination string    `json:"destination"`
	AmountCents int64     `json:"amount_cents"`
	AmountFixed bool      `json:"amount_fixed"`
	ExpiresAt   time.Time `json:"expires_at"`
	PublicKey   string    `json:"public_key"`
}

// ChargeResult is what the payer sees after a submit.
type ChargeResult struct {
	ChargeID       string `json:"charge_id"`
	State          string `json:"state"`
	LawPayChargeID string `json:"lawpay_charge_id,omitempty"`
	PaymentID      string `json:"payment_id,omitempty"`
	Code           string `json:"code,omitempty"`
	Message        string `json:"message"`
}

func outOfRange(min, max int64) *appError {
	return &appError{Status: http.StatusUnprocessableEntity, Code: "amount_out_of_range",
		Message: fmt.Sprintf("Enter an amount from %s to %s.", dollars(min), dollars(max))}
}

// dollars formats cents as $1,234.56 (negative as -$1,234.56).
func dollars(c int64) string {
	sign := ""
	if c < 0 {
		sign, c = "-", -c
	}
	whole, frac := c/100, c%100
	s := fmt.Sprintf("%d", whole)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return fmt.Sprintf("%s$%s.%02d", sign, s, frac)
}

// CreateIntent checks a pay request and stores a pay intent. Every check that needs no
// core read runs first, so an out-of-range amount makes no core or adapter call.
func (s *Server) CreateIntent(ctx context.Context, v viewer, req PayRequest) (Intent, error) {
	st, err := s.settings(ctx)
	if err != nil {
		return Intent{}, err
	}
	if !st.LawPayReady() {
		return Intent{}, errNoLawPay
	}
	in := Intent{Purpose: req.Purpose, PublicKey: st.LawPayPublic}
	contact, channel := "", "portal"
	if v.Client() {
		if v.ContactID == "" {
			return Intent{}, errNotFound
		}
		contact = v.ContactID
		switch req.Purpose {
		case "invoice_pay":
			if req.InvoiceID == "" {
				return Intent{}, errNotFound
			}
			row, found, err := s.invoicePayability(ctx, req.InvoiceID)
			if err != nil {
				return Intent{}, err
			}
			inv, err := s.core.GetInvoice(ctx, v.auth(), req.InvoiceID)
			if err != nil {
				if coreapi.Is(err, http.StatusNotFound) || coreapi.Is(err, http.StatusForbidden) {
					return Intent{}, errNotFound
				}
				return Intent{}, err
			}
			if !found || !row.Payable || inv.BalanceDueCents <= 0 {
				return Intent{}, fail(http.StatusConflict, "not_payable", msgNotPayable)
			}
			in.InvoiceID, in.MatterID, in.Destination, in.AmountCents, in.AmountFixed = inv.ID, inv.MatterID, row.Destination, inv.BalanceDueCents, true
		case "trust_deposit":
			if req.MatterID == "" {
				return Intent{}, errNotFound
			}
			enabled, minimum, found, err := s.matterPayability(ctx, req.MatterID)
			if err != nil {
				return Intent{}, err
			}
			if !found || !enabled {
				return Intent{}, errNotFound
			}
			in.MatterID, in.Destination = req.MatterID, "trust"
			if req.ReplenishmentRequestID == "" {
				floor := max(st.MinCharge, minimum)
				if req.AmountCents < floor || req.AmountCents > st.MaxCharge {
					return Intent{}, outOfRange(floor, st.MaxCharge)
				}
				in.AmountCents = req.AmountCents
			}
			if _, err := s.core.GetMatter(ctx, v.auth(), req.MatterID); err != nil {
				if coreapi.Is(err, http.StatusNotFound) || coreapi.Is(err, http.StatusForbidden) {
					return Intent{}, errNotFound
				}
				return Intent{}, err
			}
			if req.ReplenishmentRequestID != "" {
				rr, err := s.core.GetReplenishment(ctx, v.auth(), req.ReplenishmentRequestID)
				if err != nil {
					if coreapi.Is(err, http.StatusNotFound) || coreapi.Is(err, http.StatusForbidden) {
						return Intent{}, errNotFound
					}
					return Intent{}, err
				}
				if rr.MatterID != req.MatterID || (rr.ClientID != "" && rr.ClientID != contact) {
					return Intent{}, errNotFound
				}
				if rr.Status != "active" {
					return Intent{}, fail(http.StatusConflict, "not_active", "This deposit request is no longer open.")
				}
				in.AmountCents, in.AmountFixed = rr.Amount(), true
				req.ReplenishmentRequestID = rr.ID
			}
		default:
			return Intent{}, errNotFound
		}
	} else {
		if !v.canCharge() {
			return Intent{}, errForbidden
		}
		if req.AmountCents < st.MinCharge || req.AmountCents > st.MaxCharge {
			return Intent{}, outOfRange(st.MinCharge, st.MaxCharge)
		}
		if req.MatterID == "" {
			return Intent{}, fail(http.StatusUnprocessableEntity, "validation_failed", "Pick a matter.")
		}
		dest := req.Destination
		if dest == "" {
			dest = "operating"
		}
		if dest != "operating" && dest != "trust" {
			return Intent{}, fail(http.StatusUnprocessableEntity, "validation_failed", "Pick operating or trust.")
		}
		if dest == "trust" && req.InvoiceID != "" {
			return Intent{}, fail(http.StatusUnprocessableEntity, "validation_failed", "A trust deposit doesn't pay an invoice directly.")
		}
		if _, err := s.core.GetMatter(ctx, v.auth(), req.MatterID); err != nil {
			if coreapi.Is(err, http.StatusNotFound) {
				return Intent{}, errNotFound
			}
			return Intent{}, err
		}
		contact = req.ContactID
		if contact == "" {
			if contact, err = s.core.ClientParty(ctx, v.auth(), req.MatterID); err != nil {
				return Intent{}, err
			}
			if contact == "" {
				return Intent{}, fail(http.StatusUnprocessableEntity, "validation_failed", "The matter has no client to charge.")
			}
		}
		channel = "staff"
		in.Purpose, in.MatterID, in.InvoiceID, in.Destination, in.AmountCents = "staff_charge", req.MatterID, req.InvoiceID, dest, req.AmountCents
	}
	if in.AmountCents <= 0 {
		return Intent{}, fail(http.StatusConflict, "not_payable", msgNotPayable)
	}
	if in.Destination == "trust" && !s.trustReady(ctx) {
		return Intent{}, &appError{Status: http.StatusConflict, Code: "not_configured", Message: msgTrustOff, Extra: map[string]any{"settings_link": settingsLink}}
	}
	in.ID = s.id()
	in.ExpiresAt = s.now().Add(time.Duration(st.PayIntentMinutes) * time.Minute)
	_, err = s.c.DB.ExecContext(ctx, `INSERT INTO lawpay_charges (id, matter_id, contact_id, invoice_id, replenishment_request_id, purpose, channel,
		destination, amount_cents, state, initiated_by, expires_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'intent', $10, $11, $12, $12)`,
		in.ID, in.MatterID, contact, nullable(in.InvoiceID), nullable(req.ReplenishmentRequestID), in.Purpose, channel, in.Destination,
		in.AmountCents, v.UserID, in.ExpiresAt, s.now())
	if err != nil {
		return Intent{}, err
	}
	return in, s.depositsOn(ctx, in.MatterID, "")
}

// charge is a lawpay_charges row.
type charge struct {
	ID, MatterID, ContactID, InvoiceID, PaymentID, InitializationID, ReplenishmentID string
	Purpose, Channel, Destination, Method, LawPayChargeID, LawPayStatus, State       string
	FailureCode, FailureMessage, InitiatedBy                                         string
	AmountCents                                                                      int64
	Attempts                                                                         int
	ExpiresAt, CreatedAt, UpdatedAt                                                  time.Time
}

const chargeCols = `id, matter_id, contact_id, COALESCE(invoice_id, ''), COALESCE(payment_id, ''), COALESCE(initialization_id, ''),
	COALESCE(replenishment_request_id, ''), purpose, channel, destination, COALESCE(method, ''), COALESCE(lawpay_charge_id, ''),
	lawpay_status, state, failure_code, failure_message, initiated_by, amount_cents, attempts, expires_at, created_at, updated_at`

type scanner interface{ Scan(dest ...any) error }

func scanCharge(r scanner) (charge, error) {
	var c charge
	err := r.Scan(&c.ID, &c.MatterID, &c.ContactID, &c.InvoiceID, &c.PaymentID, &c.InitializationID, &c.ReplenishmentID, &c.Purpose,
		&c.Channel, &c.Destination, &c.Method, &c.LawPayChargeID, &c.LawPayStatus, &c.State, &c.FailureCode, &c.FailureMessage,
		&c.InitiatedBy, &c.AmountCents, &c.Attempts, &c.ExpiresAt, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

func (s *Server) chargeBy(ctx context.Context, col, val string) (charge, bool, error) {
	c, err := scanCharge(s.c.DB.QueryRowContext(ctx, `SELECT `+chargeCols+` FROM lawpay_charges WHERE `+col+` = $1`, val))
	if errors.Is(err, errNoRows) {
		return c, false, nil
	}
	return c, err == nil, err
}

func (c charge) result() ChargeResult {
	r := ChargeResult{ChargeID: c.ID, State: c.State, LawPayChargeID: c.LawPayChargeID, PaymentID: c.PaymentID, Code: c.FailureCode}
	switch c.State {
	case "recorded_completed":
		r.Message = "Payment received. Thank you."
	case "recorded_pending":
		r.Message = "Your bank payment is processing. It can take a few business days to clear."
	case "declined":
		r.Message = msgDeclined
	case "charged_unrecorded":
		r.Message = fmt.Sprintf(msgUnrecorded, c.LawPayChargeID)
	case "expired":
		r.Message = msgIntentExpire
	default:
		r.Message = c.FailureMessage
	}
	return r
}

func (s *Server) setCharge(ctx context.Context, id, set string, args ...any) error {
	_, err := s.c.DB.ExecContext(ctx, `UPDATE lawpay_charges SET `+set+`, updated_at = $2 WHERE id = $1`, append([]any{id, s.now()}, args...)...)
	return err
}

// Submit charges a pay intent once. A second submit of the same intent returns the first
// result; an expired intent charges nothing.
func (s *Server) Submit(ctx context.Context, v viewer, id, token string) (ChargeResult, error) {
	c, found, err := s.chargeBy(ctx, "id", id)
	if err != nil {
		return ChargeResult{}, err
	}
	if !found || (v.Client() && (c.Channel != "portal" || c.ContactID != v.ContactID)) || (!v.Client() && !v.staff()) {
		return ChargeResult{}, errNotFound
	}
	if !v.Client() && c.Channel == "staff" && !v.canCharge() {
		return ChargeResult{}, errForbidden
	}
	if c.State != "intent" {
		return s.awaitResult(ctx, c)
	}
	if !s.now().Before(c.ExpiresAt) {
		_ = s.setCharge(ctx, id, `state = 'expired', failure_code = $3`, codeExpired)
		c.State = "expired"
		return c.result(), nil
	}
	st, err := s.settings(ctx)
	if err != nil {
		return ChargeResult{}, err
	}
	if !st.LawPayReady() {
		return ChargeResult{}, errNoLawPay
	}
	if c.Destination == "trust" && !s.trustReady(ctx) {
		return ChargeResult{}, &appError{Status: http.StatusConflict, Code: "not_configured", Message: msgTrustOff, Extra: map[string]any{"settings_link": settingsLink}}
	}
	if c.Channel == "portal" {
		var n int
		since := s.now().Add(-time.Duration(st.AttemptWindowMinutes) * time.Minute)
		if err := s.c.DB.QueryRowContext(ctx, `SELECT count(*) FROM lawpay_charges WHERE contact_id = $1 AND channel = 'portal'
			AND state NOT IN ('intent', 'expired') AND updated_at > $2`, c.ContactID, since).Scan(&n); err != nil {
			return ChargeResult{}, err
		}
		if n >= st.AttemptsPerWindow {
			return ChargeResult{}, fail(http.StatusTooManyRequests, "rate_limited", msgTooMany)
		}
	}
	res, err := s.c.DB.ExecContext(ctx, `UPDATE lawpay_charges SET state = 'authorized', updated_at = $2 WHERE id = $1 AND state = 'intent'`, id, s.now())
	if err != nil {
		return ChargeResult{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		c, _, err = s.chargeBy(ctx, "id", id)
		if err != nil {
			return ChargeResult{}, err
		}
		return s.awaitResult(ctx, c)
	}
	defer s.grants.notify("charge:" + id)
	return s.run(ctx, v, st, c, token)
}

// awaitResult waits for a charge another submit is running, then returns its result.
func (s *Server) awaitResult(ctx context.Context, c charge) (ChargeResult, error) {
	err := s.grants.wait(ctx, "charge:"+c.ID, c.ExpiresAt, func() (bool, error) {
		cur, _, err := s.chargeBy(ctx, "id", c.ID)
		if err != nil {
			return false, err
		}
		c = cur
		return c.State != "authorized" && c.State != "charging", nil
	})
	if err != nil && !errors.Is(err, errGrantWait) {
		return ChargeResult{}, err
	}
	return c.result(), nil
}

func (st Settings) account(dest string) string {
	if dest == "trust" {
		return st.TrustAccount
	}
	return st.OperatingAccount
}

// run is one charge: core initialization, the grant, the LawPay charge, the core payment.
func (s *Server) run(ctx context.Context, v viewer, st Settings, c charge, token string) (ChargeResult, error) {
	body := map[string]any{"processor_module_id": ID, "contact_id": c.ContactID, "matter_id": c.MatterID,
		"amount_cents": c.AmountCents, "destination": c.Destination}
	if c.Destination == "trust" {
		body["trust_account_id"] = st.CoreTrustAccount
	} else if c.InvoiceID != "" {
		body["invoice_id"] = c.InvoiceID
	}
	if c.ReplenishmentID != "" {
		body["replenishment_request_id"] = c.ReplenishmentID
	}
	a := v.auth()
	a.Idempotency = "lpds-init-" + c.ID
	init, err := s.core.InitiateProcessor(ctx, a, body)
	if err != nil || init.Status == "denied" {
		msg := "The payment couldn't be authorized."
		if err != nil {
			msg = asAppError(err).Message
		}
		_ = s.setCharge(context.WithoutCancel(ctx), c.ID, `state = 'failed', failure_code = $3, failure_message = $4`, codeDenied, msg)
		c.State, c.FailureCode, c.FailureMessage = "failed", codeDenied, msg
		return c.result(), nil
	}
	if err := s.setCharge(ctx, c.ID, `initialization_id = $3, reserved_payment_id = $4`, init.ID, nullable(init.PaymentID)); err != nil {
		return ChargeResult{}, err
	}
	var cred string
	err = s.grants.wait(ctx, "proc:"+init.ID, c.ExpiresAt, func() (bool, error) {
		var err error
		cred, err = s.grantFor(ctx, init.ID)
		return cred != "", err
	})
	if err != nil {
		msg := "The payment couldn't be authorized in time. Nothing was charged."
		_ = s.setCharge(context.WithoutCancel(ctx), c.ID, `state = 'failed', failure_code = $3, failure_message = $4`, codeNoGrant, msg)
		c.State, c.FailureCode, c.FailureMessage = "failed", codeNoGrant, msg
		return c.result(), nil
	}
	if err := s.setCharge(ctx, c.ID, `state = 'charging', attempts = attempts + 1`); err != nil {
		return ChargeResult{}, err
	}
	bg := context.WithoutCancel(ctx)
	tx, err := s.lawpay(st).Charge(bg, adapters.ChargeRequest{AccountID: st.account(c.Destination), TokenID: token, Reference: c.ID, AmountCents: c.AmountCents})
	if err != nil {
		if adapters.Declined(err) {
			_ = s.setCharge(bg, c.ID, `state = 'declined', failure_code = $3, failure_message = $4`, codeDeclined, lawpayMessage(err))
			c.State, c.FailureCode = "declined", codeDeclined
			return c.result(), nil
		}
		msg := "LawPay didn't confirm the charge. Check LawPay for reference " + c.ID + " before trying again."
		_ = s.setCharge(bg, c.ID, `state = 'failed', failure_code = $3, failure_message = $4`, codeError, msg)
		c.State, c.FailureCode, c.FailureMessage = "failed", codeError, msg
		return c.result(), nil
	}
	c.LawPayChargeID, c.LawPayStatus, c.Method = tx.ID, tx.Status, adapters.Method(tx.MethodType)
	outcome := adapters.Map(tx, false)
	if err := s.setCharge(bg, c.ID, `lawpay_charge_id = $3, lawpay_status = $4, method = $5`, tx.ID, tx.Status, c.Method); err != nil {
		return ChargeResult{}, err
	}
	if outcome == adapters.OutcomeDeclined || outcome == adapters.OutcomeFailed {
		_ = s.setCharge(bg, c.ID, `state = 'declined', failure_code = $3, failure_message = $4`, codeDeclined, msgDeclined)
		c.State, c.FailureCode = "declined", codeDeclined
		return c.result(), nil
	}
	status := "completed"
	if outcome == adapters.OutcomePending {
		status = "pending"
	}
	return s.record(bg, c, st, cred, status)
}

func lawpayMessage(err error) string {
	var le *adapters.LawPayError
	if errors.As(err, &le) && le.Message != "" {
		return le.Message
	}
	return "LawPay refused the request."
}

// record writes the core payment for a LawPay charge with its grant and the charge's own
// idempotency key, so a retry never records twice.
func (s *Server) record(ctx context.Context, c charge, st Settings, cred, status string) (ChargeResult, error) {
	body := map[string]any{"contact_id": c.ContactID, "matter_id": c.MatterID, "amount_cents": c.AmountCents,
		"destination": c.Destination, "method": c.Method, "reference_number": c.LawPayChargeID, "status": status, "source": ID}
	if c.Destination == "trust" {
		body["trust_account_id"] = st.CoreTrustAccount
	} else if c.InvoiceID != "" {
		body["invoice_id"] = c.InvoiceID
	}
	if c.ReplenishmentID != "" {
		body["replenishment_request_id"] = c.ReplenishmentID
	}
	p, err := s.core.CreatePayment(ctx, coreapi.Auth{Task: cred, Idempotency: "lpds-charge-" + c.ID}, body)
	if err != nil {
		s.c.Logf("lawpay-docuseal: charge %s (LawPay %s) not recorded: %v", c.ID, c.LawPayChargeID, err)
		if e := s.setCharge(ctx, c.ID, `state = 'charged_unrecorded', failure_code = $3, failure_message = $4, attempts = 1`,
			codeUnrecorded, fmt.Sprintf(msgUnrecorded, c.LawPayChargeID)); e != nil {
			return ChargeResult{}, e
		}
		c.State, c.FailureCode = "charged_unrecorded", codeUnrecorded
		return c.result(), nil
	}
	state := "recorded_completed"
	if p.Status == "pending" {
		state = "recorded_pending"
	}
	if err := s.setCharge(ctx, c.ID, `state = $3, payment_id = $4, failure_code = '', failure_message = ''`, state, p.ID); err != nil {
		return ChargeResult{}, err
	}
	c.State, c.PaymentID, c.FailureCode = state, p.ID, ""
	if state == "recorded_completed" {
		s.funded(ctx, c)
	}
	return c.result(), nil
}

// funded marks an invoice paid through trust as funded: the money is in trust and staff
// apply it to the invoice by a deliberate act.
func (s *Server) funded(ctx context.Context, c charge) {
	if c.Destination != "trust" || c.InvoiceID == "" {
		return
	}
	if _, err := s.c.DB.ExecContext(ctx, `UPDATE payable_invoices SET status = 'funded', funded_cents = funded_cents + $2
		WHERE invoice_id = $1 AND status IN ('open', 'funded')`, c.InvoiceID, c.AmountCents); err != nil {
		s.c.Logf("lawpay-docuseal: invoice %s not marked funded: %v", c.InvoiceID, err)
	}
}

// settle moves a recorded pending charge to completed or failed in the core with its grant
// (the pending grant lifetime, C4).
func (s *Server) settle(ctx context.Context, c charge, st Settings, outcome adapters.Outcome, lawpayStatus string) error {
	status, state := "completed", "recorded_completed"
	if outcome == adapters.OutcomeFailed || outcome == adapters.OutcomeDeclined {
		status, state = "failed", "failed"
	}
	cred, err := s.grantFor(ctx, c.InitializationID)
	if err != nil {
		return err
	}
	if _, err := s.core.PaymentStatus(ctx, coreapi.Auth{Task: cred}, c.PaymentID, status); err != nil {
		return err
	}
	set := `state = $3, lawpay_status = $4`
	args := []any{state, lawpayStatus}
	if state == "failed" {
		set += `, failure_code = $5, failure_message = $6`
		args = append(args, codeFailed, "The bank payment didn't go through.")
	}
	if err := s.setCharge(ctx, c.ID, set, args...); err != nil {
		return err
	}
	if state == "recorded_completed" {
		c.State = state
		s.funded(ctx, c)
	}
	_ = st
	return nil
}
