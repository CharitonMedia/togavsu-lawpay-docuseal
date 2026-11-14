// Package adapters holds the LawPay and DocuSeal interfaces the module calls, the HTTP
// adapters behind them (built only from admin-entered settings at runtime), the one
// table-driven LawPay status map, and integer-cents parsing.
package adapters

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// LawPay is the LawPay gateway (Unified's src/lib/lawpay.ts): charge an account, refund all
// or part of a charge, read a transaction.
type LawPay interface {
	Charge(ctx context.Context, r ChargeRequest) (Transaction, error)              // POST /charges
	Refund(ctx context.Context, chargeID string, cents int64) (Transaction, error) // POST /charges/{id}/refund
	Transaction(ctx context.Context, id string) (Transaction, error)               // GET /transactions/{id}
}

// ChargeRequest charges one LawPay account. TokenID is the hosted fields token; card or bank
// data never reaches the module. Reference is the module's charge id.
type ChargeRequest struct {
	AccountID, TokenID, Reference string
	AmountCents                   int64
}

// Transaction is a LawPay transaction as the module needs it. Type is CHARGE or REFUND;
// MethodType is card or bank; ChargeID is set on a refund.
type Transaction struct {
	ID, Type, Status, AccountID, ChargeID, MethodType string
	AmountCents                                       int64
}

// LawPayError is a non-2xx LawPay answer: LawPay's message and code, and the HTTP status.
type LawPayError struct {
	Message, Code string
	HTTPStatus    int
}

func (e *LawPayError) Error() string {
	return fmt.Sprintf("lawpay %d %s: %s", e.HTTPStatus, e.Code, e.Message)
}

// NotFound reports whether err is LawPay's 404 for an unknown id.
func NotFound(err error) bool {
	var le *LawPayError
	return errors.As(err, &le) && le.HTTPStatus == http.StatusNotFound
}

// Declined reports whether err is LawPay refusing the card or bank account (as opposed to
// a network or server error).
func Declined(err error) bool {
	var le *LawPayError
	if !errors.As(err, &le) {
		return false
	}
	return le.HTTPStatus == http.StatusPaymentRequired || le.HTTPStatus == http.StatusUnprocessableEntity ||
		strings.Contains(strings.ToLower(le.Code), "declin")
}

// LawPayHTTP is the live adapter: Basic auth with the secret key and an empty password,
// amounts as the decimal string of integer cents.
type LawPayHTTP struct {
	Base, SecretKey string
	HTTP            *http.Client
}

// NewLawPayHTTP builds the live adapter from settings.
func NewLawPayHTTP(base, secret string, c *http.Client) *LawPayHTTP {
	if c == nil {
		c = http.DefaultClient
	}
	return &LawPayHTTP{Base: strings.TrimRight(base, "/"), SecretKey: secret, HTTP: c}
}

type lawpayTx struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Status    string          `json:"status"`
	AccountID string          `json:"account_id"`
	ChargeID  string          `json:"charge_id"`
	Amount    json.RawMessage `json:"amount"`
	Method    struct {
		Type string `json:"type"`
	} `json:"method"`
}

func (t lawpayTx) transaction() (Transaction, error) {
	cents, err := ParseCentsJSON(t.Amount)
	if err != nil {
		return Transaction{}, err
	}
	return Transaction{ID: t.ID, Type: strings.ToUpper(t.Type), Status: strings.ToUpper(t.Status), AccountID: t.AccountID,
		ChargeID: t.ChargeID, MethodType: strings.ToLower(t.Method.Type), AmountCents: cents}, nil
}

func (l *LawPayHTTP) do(ctx context.Context, method, path string, body any) (Transaction, error) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return Transaction{}, err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, l.Base+path, rd)
	if err != nil {
		return Transaction{}, err
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(l.SecretKey+":")))
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := l.HTTP.Do(req)
	if err != nil {
		return Transaction{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Transaction{}, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		var e struct {
			Messages []struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"messages"`
		}
		le := &LawPayError{HTTPStatus: resp.StatusCode, Message: resp.Status}
		if json.Unmarshal(raw, &e) == nil && len(e.Messages) > 0 {
			le.Code, le.Message = e.Messages[0].Code, e.Messages[0].Message
		}
		return Transaction{}, le
	}
	var tx lawpayTx
	if err := json.Unmarshal(raw, &tx); err != nil {
		return Transaction{}, err
	}
	return tx.transaction()
}

// Charge charges AmountCents to AccountID with the hosted fields token.
func (l *LawPayHTTP) Charge(ctx context.Context, r ChargeRequest) (Transaction, error) {
	return l.do(ctx, http.MethodPost, "/charges", map[string]string{"amount": strconv.FormatInt(r.AmountCents, 10),
		"account_id": r.AccountID, "method": r.TokenID, "reference": r.Reference})
}

// Refund refunds cents of a charge.
func (l *LawPayHTTP) Refund(ctx context.Context, chargeID string, cents int64) (Transaction, error) {
	return l.do(ctx, http.MethodPost, "/charges/"+url.PathEscape(chargeID)+"/refund", map[string]string{"amount": strconv.FormatInt(cents, 10)})
}

// Transaction reads one transaction.
func (l *LawPayHTTP) Transaction(ctx context.Context, id string) (Transaction, error) {
	return l.do(ctx, http.MethodGet, "/transactions/"+url.PathEscape(id), nil)
}
