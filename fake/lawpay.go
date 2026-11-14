// Package fake holds in-memory LawPay and DocuSeal stand-ins with scripted outcomes. Every
// test uses them; nothing here opens a network connection. They also generate the webhook
// bodies the real services send, so tests drive the webhook receivers with realistic
// payloads. Placeholder tokens such as tok_test_card and tok_test_bank stand in for the
// hosted fields tokens; no card or bank data exists anywhere.
package fake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/CharitonMedia/togavsu-lawpay-docuseal/adapters"
)

// Placeholder hosted fields tokens.
const (
	CardToken = "tok_test_card"
	BankToken = "tok_test_bank"
)

// ErrNetwork is a scripted network failure.
var ErrNetwork = errors.New("fake: network error")

// LawPay is an in-memory LawPay. The zero value is not usable; call NewLawPay.
type LawPay struct {
	mu   sync.Mutex
	seq  int
	txs  map[string]adapters.Transaction
	next []string // scripted outcomes for the next calls: decline, network, refund_fail, read_network

	// Calls made, by kind.
	ChargeCalls, RefundCalls, ReadCalls int
	Charges                             []adapters.ChargeRequest
}

// NewLawPay returns an empty fake.
func NewLawPay() *LawPay { return &LawPay{txs: map[string]adapters.Transaction{}} }

// Script queues outcomes for the next calls, in order: "decline" or "network" for a charge,
// "refund_fail" or "network" for a refund, "read_network" or "read_404" for a read.
func (f *LawPay) Script(outcomes ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next = append(f.next, outcomes...)
}

func (f *LawPay) take(kinds ...string) string {
	if len(f.next) == 0 {
		return ""
	}
	for _, k := range kinds {
		if f.next[0] == k {
			f.next = f.next[1:]
			return k
		}
	}
	return ""
}

func (f *LawPay) id(prefix string) string {
	f.seq++
	return fmt.Sprintf("%s_%04d", prefix, f.seq)
}

// Charge charges the account. A bank token is authorized and settles later; a card token
// completes at once.
func (f *LawPay) Charge(_ context.Context, r adapters.ChargeRequest) (adapters.Transaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ChargeCalls++
	f.Charges = append(f.Charges, r)
	switch f.take("decline", "network") {
	case "decline":
		return adapters.Transaction{}, &adapters.LawPayError{Message: "The card was declined.", Code: "card_declined", HTTPStatus: http.StatusPaymentRequired}
	case "network":
		return adapters.Transaction{}, ErrNetwork
	}
	tx := adapters.Transaction{ID: f.id("ch"), Type: "CHARGE", AccountID: r.AccountID, AmountCents: r.AmountCents}
	if r.TokenID == BankToken {
		tx.MethodType, tx.Status = "bank", "AUTHORIZED"
	} else {
		tx.MethodType, tx.Status = "card", "COMPLETED"
	}
	f.txs[tx.ID] = tx
	return tx, nil
}

// Refund refunds cents of a charge.
func (f *LawPay) Refund(_ context.Context, chargeID string, cents int64) (adapters.Transaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.RefundCalls++
	switch f.take("refund_fail", "network") {
	case "refund_fail":
		return adapters.Transaction{}, &adapters.LawPayError{Message: "The refund could not be processed.", Code: "refund_failed", HTTPStatus: http.StatusUnprocessableEntity}
	case "network":
		return adapters.Transaction{}, ErrNetwork
	}
	ch, ok := f.txs[chargeID]
	if !ok {
		return adapters.Transaction{}, &adapters.LawPayError{Message: "Not found", Code: "not_found", HTTPStatus: http.StatusNotFound}
	}
	tx := adapters.Transaction{ID: f.id("rf"), Type: "REFUND", Status: "COMPLETED", AccountID: ch.AccountID, ChargeID: chargeID,
		MethodType: ch.MethodType, AmountCents: cents}
	f.txs[tx.ID] = tx
	return tx, nil
}

// Transaction reads one transaction; an unknown id is LawPay's 404.
func (f *LawPay) Transaction(_ context.Context, id string) (adapters.Transaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ReadCalls++
	switch f.take("read_network", "read_404") {
	case "read_network":
		return adapters.Transaction{}, ErrNetwork
	case "read_404":
		return adapters.Transaction{}, &adapters.LawPayError{Message: "Not found", Code: "not_found", HTTPStatus: http.StatusNotFound}
	}
	tx, ok := f.txs[id]
	if !ok {
		return adapters.Transaction{}, &adapters.LawPayError{Message: "Not found", Code: "not_found", HTTPStatus: http.StatusNotFound}
	}
	return tx, nil
}

// Settle moves a transaction to status (delayed settlement or a later failure).
func (f *LawPay) Settle(id, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tx := f.txs[id]
	tx.Status = status
	f.txs[id] = tx
}

// Get returns a transaction as stored.
func (f *LawPay) Get(id string) (adapters.Transaction, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tx, ok := f.txs[id]
	return tx, ok
}

// AddCharge puts a charge into LawPay that the module never made (an unknown transaction).
func (f *LawPay) AddCharge(accountID, methodType string, cents int64) adapters.Transaction {
	f.mu.Lock()
	defer f.mu.Unlock()
	tx := adapters.Transaction{ID: f.id("ch"), Type: "CHARGE", Status: "COMPLETED", AccountID: accountID, MethodType: methodType, AmountCents: cents}
	f.txs[tx.ID] = tx
	return tx
}

// DashboardRefund refunds a charge in the LawPay dashboard, outside the module.
func (f *LawPay) DashboardRefund(chargeID string, cents int64) adapters.Transaction {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := f.txs[chargeID]
	tx := adapters.Transaction{ID: f.id("rf"), Type: "REFUND", Status: "COMPLETED", AccountID: ch.AccountID, ChargeID: chargeID,
		MethodType: ch.MethodType, AmountCents: cents}
	f.txs[tx.ID] = tx
	return tx
}

// Calls returns the number of adapter calls of every kind so far.
func (f *LawPay) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ChargeCalls + f.RefundCalls + f.ReadCalls
}

// ChargeWebhook is the body LawPay posts for a charge transaction's status.
func ChargeWebhook(tx adapters.Transaction) []byte {
	b, _ := json.Marshal(map[string]any{"event": "transaction." + strings.ToLower(tx.Status),
		"data": map[string]any{"id": tx.ID, "type": "CHARGE", "status": tx.Status, "account_id": tx.AccountID,
			"amount": tx.AmountCents, "method": map[string]string{"type": tx.MethodType}}})
	return b
}

// RefundWebhook is the body LawPay posts for a refund. It carries the refunded charge's id
// at the top level too, which is the trap the receiver must not fall into: a refund body
// never completes the charge it names.
func RefundWebhook(tx adapters.Transaction) []byte {
	b, _ := json.Marshal(map[string]any{"id": tx.ID, "type": "REFUND", "status": tx.Status, "charge_id": tx.ChargeID,
		"account_id": tx.AccountID, "amount": tx.AmountCents})
	return b
}
