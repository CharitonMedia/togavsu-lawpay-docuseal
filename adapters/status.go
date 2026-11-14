package adapters

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// Outcome is what a LawPay transaction means for the module's records.
type Outcome string

// Outcomes.
const (
	// OutcomeCompleted: record (or complete) the core payment as completed.
	OutcomeCompleted Outcome = "completed"
	// OutcomePending: record the core payment as pending (ACH authorized, not settled).
	OutcomePending Outcome = "pending"
	// OutcomeDeclined: the charge was refused before anything was recorded.
	OutcomeDeclined Outcome = "declined"
	// OutcomeFailed: a pending charge failed after it was recorded.
	OutcomeFailed Outcome = "failed"
	// OutcomeRefundDone: a refund LawPay accepted.
	OutcomeRefundDone Outcome = "refund_done"
	// OutcomeNone: nothing to do (an intermediate or unknown status).
	OutcomeNone Outcome = "none"
)

// statusRule is one row of the status map. Method "" matches any method; Pending tells a
// first answer (false) from a status update on a charge already recorded pending (true).
type statusRule struct {
	Type, Status, Method string
	Pending              bool
	Outcome              Outcome
}

// statusMap is the one table every LawPay status goes through (SPEC section 5).
var statusMap = []statusRule{
	{Type: "CHARGE", Status: "AUTHORIZED", Method: "card", Outcome: OutcomeCompleted},
	{Type: "CHARGE", Status: "COMPLETED", Method: "card", Outcome: OutcomeCompleted},
	{Type: "CHARGE", Status: "SETTLED", Method: "card", Outcome: OutcomeCompleted},
	{Type: "CHARGE", Status: "AUTHORIZED", Method: "bank", Outcome: OutcomePending},
	{Type: "CHARGE", Status: "PENDING", Method: "bank", Outcome: OutcomePending},
	{Type: "CHARGE", Status: "COMPLETED", Method: "bank", Outcome: OutcomeCompleted},
	{Type: "CHARGE", Status: "SETTLED", Method: "bank", Outcome: OutcomeCompleted},
	{Type: "CHARGE", Status: "AUTHORIZED", Pending: true, Outcome: OutcomeNone},
	{Type: "CHARGE", Status: "PENDING", Pending: true, Outcome: OutcomeNone},
	{Type: "CHARGE", Status: "COMPLETED", Pending: true, Outcome: OutcomeCompleted},
	{Type: "CHARGE", Status: "SETTLED", Pending: true, Outcome: OutcomeCompleted},
	{Type: "CHARGE", Status: "DECLINED", Outcome: OutcomeDeclined},
	{Type: "CHARGE", Status: "FAILED", Outcome: OutcomeDeclined},
	{Type: "CHARGE", Status: "VOIDED", Outcome: OutcomeDeclined},
	{Type: "CHARGE", Status: "DECLINED", Pending: true, Outcome: OutcomeFailed},
	{Type: "CHARGE", Status: "FAILED", Pending: true, Outcome: OutcomeFailed},
	{Type: "CHARGE", Status: "VOIDED", Pending: true, Outcome: OutcomeFailed},
	{Type: "REFUND", Status: "AUTHORIZED", Outcome: OutcomeRefundDone},
	{Type: "REFUND", Status: "COMPLETED", Outcome: OutcomeRefundDone},
	{Type: "REFUND", Status: "SETTLED", Outcome: OutcomeRefundDone},
}

// Map returns the outcome of tx. pending says the module's charge row is already recorded
// pending, so the answer is a status update rather than the first charge result.
func Map(tx Transaction, pending bool) Outcome {
	typ := strings.ToUpper(tx.Type)
	if typ == "" {
		typ = "CHARGE"
	}
	status, method := strings.ToUpper(tx.Status), strings.ToLower(tx.MethodType)
	for _, r := range statusMap {
		if r.Type != typ || r.Status != status || r.Pending != pending {
			continue
		}
		if r.Method != "" && r.Method != method {
			continue
		}
		return r.Outcome
	}
	return OutcomeNone
}

// Method maps LawPay's method type to the core payment method: card, or ach for a bank
// account.
func Method(methodType string) string {
	if strings.EqualFold(methodType, "bank") || strings.EqualFold(methodType, "ach") {
		return "ach"
	}
	return "card"
}

// ErrCents is an amount that is not a non-negative whole number of cents.
var ErrCents = errors.New("adapters: an amount is a non-negative whole number of cents")

// ParseCents reads an integer number of cents. A fraction, a sign, an exponent or anything
// else that isn't digits is refused, so no amount is ever converted through dollars.
func ParseCents(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, ErrCents
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, ErrCents
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, ErrCents
	}
	return v, nil
}

// ParseCentsJSON reads a cents value that arrives as a JSON number or a JSON string. An
// absent value is zero.
func ParseCentsJSON(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return ParseCents(s)
	}
	return ParseCents(string(raw))
}
