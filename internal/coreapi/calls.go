package coreapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// ViewerInfo is the core's answer to context-token verification.
type ViewerInfo struct {
	UserID   string   `json:"viewer_user_id"`
	Roles    []string `json:"roles"`
	Module   string   `json:"module"`
	MatterID *string  `json:"matter_id"`
	// ContactID is a client viewer's linked contact. The verify answer must carry it for
	// client tokens; no CORE item names it yet (see the README's core gaps).
	ContactID string `json:"contact_id"`
}

// Has reports whether the viewer holds any of roles.
func (v ViewerInfo) Has(roles ...string) bool {
	for _, have := range v.Roles {
		for _, want := range roles {
			if have == want {
				return true
			}
		}
	}
	return false
}

// Client reports whether the viewer is a client-only user.
func (v ViewerInfo) Client() bool {
	return v.Has("client") && !v.Has("admin", "attorney", "staff", "accountant")
}

// Matter returns the context's matter id, or "".
func (v ViewerInfo) Matter() string {
	if v.MatterID == nil {
		return ""
	}
	return *v.MatterID
}

// Verify checks a frame's context token.
func (c *Client) Verify(ctx context.Context, token string) (ViewerInfo, error) {
	var v ViewerInfo
	err := c.Do(ctx, http.MethodPost, Path("/api/v1/modules", c.Module, "context-tokens", "verify"), Viewer(token), nil, &v)
	return v, err
}

// SettingValue is one module setting as the module's own token reads it (secrets included).
type SettingValue struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Settings reads the module's settings.
func (c *Client) Settings(ctx context.Context) (map[string]string, error) {
	var rows []SettingValue
	if err := c.Do(ctx, http.MethodGet, Path("/api/v1/modules", c.Module, "settings"), Service, nil, &rows); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, r := range rows {
		out[r.Key] = r.Value
	}
	return out, nil
}

// Matter is the core matter fields the module reads.
type Matter struct {
	ID                        string  `json:"id"`
	Number                    string  `json:"number"`
	Name                      string  `json:"name"`
	Status                    string  `json:"status"`
	CaseNumber                string  `json:"case_number"`
	BillingType               *string `json:"billing_type"`
	FlatFeeCents              *int64  `json:"flat_fee_cents"`
	ResponsibleAttorneyUserID *string `json:"responsible_attorney_user_id"`
}

// GetMatter reads a matter.
func (c *Client) GetMatter(ctx context.Context, a Auth, id string) (Matter, error) {
	var m Matter
	err := c.Do(ctx, http.MethodGet, Path("/api/v1/matters", id), a, nil, &m)
	return m, err
}

// PatchMatter changes matter fields.
func (c *Client) PatchMatter(ctx context.Context, a Auth, id string, fields map[string]any) (Matter, error) {
	var m Matter
	err := c.Do(ctx, http.MethodPatch, Path("/api/v1/matters", id), a, fields, &m)
	return m, err
}

// Party is a matter party.
type Party struct {
	ID        string `json:"id"`
	MatterID  string `json:"matter_id"`
	ContactID string `json:"contact_id"`
	Role      string `json:"role"`
	Primary   bool   `json:"primary"`
}

type page[T any] struct {
	Data []T `json:"data"`
}

// Parties lists a matter's parties.
func (c *Client) Parties(ctx context.Context, a Auth, matter string) ([]Party, error) {
	var p page[Party]
	err := c.Do(ctx, http.MethodGet, "/api/v1/matter-parties?"+url.Values{"matter_id": {matter}}.Encode(), a, nil, &p)
	return p.Data, err
}

// ClientParty returns the matter's primary client party, or the first client party.
func (c *Client) ClientParty(ctx context.Context, a Auth, matter string) (string, error) {
	ps, err := c.Parties(ctx, a, matter)
	if err != nil {
		return "", err
	}
	first := ""
	for _, p := range ps {
		if p.Role != "client" {
			continue
		}
		if p.Primary {
			return p.ContactID, nil
		}
		if first == "" {
			first = p.ContactID
		}
	}
	return first, nil
}

// FeeAgreement is the core fee agreement.
type FeeAgreement struct {
	ID                  string  `json:"id"`
	MatterID            string  `json:"matter_id"`
	FeeType             string  `json:"fee_type"`
	AmountCents         *int64  `json:"amount_cents"`
	ServicesDescription string  `json:"services_description"`
	Status              string  `json:"status"`
	SignedOn            *string `json:"signed_on"`
	VoidReason          string  `json:"void_reason"`
	SignedFileID        *string `json:"signed_file_id"`
}

// FeeAgreements lists a matter's fee agreements.
func (c *Client) FeeAgreements(ctx context.Context, a Auth, matter string) ([]FeeAgreement, error) {
	var p page[FeeAgreement]
	err := c.Do(ctx, http.MethodGet, "/api/v1/fee-agreements?"+url.Values{"matter_id": {matter}}.Encode(), a, nil, &p)
	return p.Data, err
}

// GetFeeAgreement reads one.
func (c *Client) GetFeeAgreement(ctx context.Context, a Auth, id string) (FeeAgreement, error) {
	var f FeeAgreement
	err := c.Do(ctx, http.MethodGet, Path("/api/v1/fee-agreements", id), a, nil, &f)
	return f, err
}

// CreateFeeAgreement creates a draft.
func (c *Client) CreateFeeAgreement(ctx context.Context, a Auth, body map[string]any) (FeeAgreement, error) {
	var f FeeAgreement
	err := c.Do(ctx, http.MethodPost, "/api/v1/fee-agreements", a, body, &f)
	return f, err
}

// PatchFeeAgreement changes one.
func (c *Client) PatchFeeAgreement(ctx context.Context, a Auth, id string, body map[string]any) (FeeAgreement, error) {
	var f FeeAgreement
	err := c.Do(ctx, http.MethodPatch, Path("/api/v1/fee-agreements", id), a, body, &f)
	return f, err
}

// Contact is the core contact fields the module reads.
type Contact struct {
	ID               string `json:"id"`
	Kind             string `json:"kind"`
	GivenName        string `json:"given_name"`
	FamilyName       string `json:"family_name"`
	OrganizationName string `json:"organization_name"`
	FullName         string `json:"full_name"`
}

// Name is the contact's display name.
func (c Contact) Name() string {
	switch {
	case c.FullName != "":
		return c.FullName
	case c.Kind == "organization" && c.OrganizationName != "":
		return c.OrganizationName
	}
	n := c.GivenName
	if c.FamilyName != "" {
		if n != "" {
			n += " "
		}
		n += c.FamilyName
	}
	return n
}

// GetContact reads a contact.
func (c *Client) GetContact(ctx context.Context, a Auth, id string) (Contact, error) {
	var ct Contact
	err := c.Do(ctx, http.MethodGet, Path("/api/v1/contacts", id), a, nil, &ct)
	return ct, err
}

// ContactEmail, ContactPhone and ContactAddress are the contact detail rows.
type ContactEmail struct {
	Email       string  `json:"email"`
	Primary     bool    `json:"primary"`
	EffectiveTo *string `json:"effective_to"`
}

// ContactPhone is a phone row.
type ContactPhone struct {
	E164        string  `json:"e164"`
	Primary     bool    `json:"primary"`
	EffectiveTo *string `json:"effective_to"`
}

// ContactAddress is an address row.
type ContactAddress struct {
	Line1       string  `json:"line1"`
	Line2       string  `json:"line2"`
	City        string  `json:"city"`
	Region      string  `json:"region"`
	PostalCode  string  `json:"postal_code"`
	Primary     bool    `json:"primary"`
	EffectiveTo *string `json:"effective_to"`
}

// Details are a contact's current primary email, phone and address, read at send time.
type Details struct {
	Email, Phone string
	Address      ContactAddress
}

// ContactDetails reads a contact's current primary email, phone and address.
func (c *Client) ContactDetails(ctx context.Context, a Auth, contact string) (Details, error) {
	q := "?" + url.Values{"contact_id": {contact}}.Encode()
	var d Details
	var emails page[ContactEmail]
	if err := c.Do(ctx, http.MethodGet, "/api/v1/contact-emails"+q, a, nil, &emails); err != nil {
		return d, err
	}
	for _, e := range emails.Data {
		if e.EffectiveTo == nil && (e.Primary || d.Email == "") {
			d.Email = e.Email
		}
	}
	var phones page[ContactPhone]
	if err := c.Do(ctx, http.MethodGet, "/api/v1/contact-phones"+q, a, nil, &phones); err != nil {
		return d, err
	}
	for _, p := range phones.Data {
		if p.EffectiveTo == nil && (p.Primary || d.Phone == "") {
			d.Phone = p.E164
		}
	}
	var addrs page[ContactAddress]
	if err := c.Do(ctx, http.MethodGet, "/api/v1/contact-addresses"+q, a, nil, &addrs); err != nil {
		return d, err
	}
	found := false
	for _, ad := range addrs.Data {
		if ad.EffectiveTo == nil && (ad.Primary || !found) {
			d.Address, found = ad, true
		}
	}
	return d, nil
}

// Invoice is the core invoice fields the module reads.
type Invoice struct {
	ID              string `json:"id"`
	MatterID        string `json:"matter_id"`
	Number          string `json:"number"`
	Status          string `json:"status"`
	TotalCents      int64  `json:"total_cents"`
	AmountPaidCents int64  `json:"amount_paid_cents"`
	BalanceDueCents int64  `json:"balance_due_cents"`
}

// GetInvoice reads an invoice (as the viewer, or as the books reader on a service call).
func (c *Client) GetInvoice(ctx context.Context, a Auth, id string) (Invoice, error) {
	var inv Invoice
	err := c.Do(ctx, http.MethodGet, Path("/api/v1/invoices", id), a, nil, &inv)
	return inv, err
}

// Invoices lists a matter's invoices.
func (c *Client) Invoices(ctx context.Context, a Auth, matter string) ([]Invoice, error) {
	var p page[Invoice]
	err := c.Do(ctx, http.MethodGet, "/api/v1/invoices?"+url.Values{"matter_id": {matter}}.Encode(), a, nil, &p)
	return p.Data, err
}

// Payment is the core payment.
type Payment struct {
	ID              string  `json:"id"`
	CreatedAt       string  `json:"created_at"`
	ContactID       string  `json:"contact_id"`
	MatterID        string  `json:"matter_id"`
	AmountCents     int64   `json:"amount_cents"`
	Destination     string  `json:"destination"`
	Method          string  `json:"method"`
	ReferenceNumber string  `json:"reference_number"`
	Status          string  `json:"status"`
	InvoiceID       *string `json:"invoice_id"`
	TrustAccountID  *string `json:"trust_account_id"`
	TrustEntryID    *string `json:"trust_entry_id"`
	Source          string  `json:"source"`
	RefundedCents   int64   `json:"refunded_cents"`
	RefundState     string  `json:"refund_state"`
}

// GetPayment reads a payment.
func (c *Client) GetPayment(ctx context.Context, a Auth, id string) (Payment, error) {
	var p Payment
	err := c.Do(ctx, http.MethodGet, Path("/api/v1/payments", id), a, nil, &p)
	return p, err
}

// Payments lists payments with the query (matter_id, status, ...).
func (c *Client) Payments(ctx context.Context, a Auth, q url.Values) ([]Payment, error) {
	var p page[Payment]
	err := c.Do(ctx, http.MethodGet, "/api/v1/payments?"+q.Encode(), a, nil, &p)
	return p.Data, err
}

// Initialization is the core's processor initialization.
type Initialization struct {
	ID        string `json:"id"`
	PaymentID string `json:"payment_id"`
	Status    string `json:"status"`
	ExpiresAt string `json:"expires_at"`
}

// InitiateProcessor asks the core to authorize this module for one payment (CORE-23).
func (c *Client) InitiateProcessor(ctx context.Context, a Auth, body map[string]any) (Initialization, error) {
	var in Initialization
	err := c.Do(ctx, http.MethodPost, "/api/v1/payments/processor-initializations", a, body, &in)
	return in, err
}

// CreatePayment records a payment.
func (c *Client) CreatePayment(ctx context.Context, a Auth, body map[string]any) (Payment, error) {
	var p Payment
	err := c.Do(ctx, http.MethodPost, "/api/v1/payments", a, body, &p)
	return p, err
}

// PaymentStatus moves a pending payment to completed or failed.
func (c *Client) PaymentStatus(ctx context.Context, a Auth, id, status string) (Payment, error) {
	var p Payment
	err := c.Do(ctx, http.MethodPost, Path("/api/v1/payments", id, "status"), a, map[string]string{"status": status}, &p)
	return p, err
}

// Refund is the core payment refund (CORE-24).
type Refund struct {
	ID              string `json:"id"`
	PaymentID       string `json:"payment_id"`
	AmountCents     int64  `json:"amount_cents"`
	Reason          string `json:"reason"`
	ReferenceNumber string `json:"reference_number"`
	Status          string `json:"status"`
	Source          string `json:"source"`
}

// CreateRefund creates a refund on a payment.
func (c *Client) CreateRefund(ctx context.Context, a Auth, payment string, body map[string]any) (Refund, error) {
	var r Refund
	err := c.Do(ctx, http.MethodPost, Path("/api/v1/payments", payment, "refunds"), a, body, &r)
	return r, err
}

// RefundStatus moves a pending refund to completed or failed.
func (c *Client) RefundStatus(ctx context.Context, a Auth, payment, refund, status string, extra map[string]any) (Refund, error) {
	body := map[string]any{"status": status}
	for k, v := range extra {
		body[k] = v
	}
	var r Refund
	err := c.Do(ctx, http.MethodPost, Path("/api/v1/payments", payment, "refunds", refund, "status"), a, body, &r)
	return r, err
}

// Refunds lists a payment's refunds.
func (c *Client) Refunds(ctx context.Context, a Auth, payment string) ([]Refund, error) {
	var p page[Refund]
	err := c.Do(ctx, http.MethodGet, Path("/api/v1/payments", payment, "refunds"), a, nil, &p)
	return p.Data, err
}

// TrustAccount is a core trust account.
type TrustAccount struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Active bool   `json:"active"`
}

// GetTrustAccount reads a trust account.
func (c *Client) GetTrustAccount(ctx context.Context, a Auth, id string) (TrustAccount, error) {
	var t TrustAccount
	err := c.Do(ctx, http.MethodGet, Path("/api/v1/trust-accounts", id), a, nil, &t)
	return t, err
}

// Balances are a matter's, client's and account's trust balances.
type Balances struct {
	MatterCents  int64 `json:"matter_cents"`
	ClientCents  int64 `json:"client_cents"`
	AccountCents int64 `json:"account_cents"`
}

// TrustBalances reads the trust balance for a matter and client.
func (c *Client) TrustBalances(ctx context.Context, a Auth, matter, client, account string) (Balances, error) {
	q := url.Values{"matter_id": {matter}, "client_id": {client}}
	if account != "" {
		q.Set("trust_account_id", account)
	}
	var p page[Balances]
	if err := c.Do(ctx, http.MethodGet, "/api/v1/trust/balances?"+q.Encode(), a, nil, &p); err != nil {
		return Balances{}, err
	}
	if len(p.Data) == 0 {
		return Balances{}, nil
	}
	return p.Data[0], nil
}

// Replenishment is a trust replenishment request. A client read carries amount_cents (the
// CORE-12 allowlist); a staff read carries amount_requested_cents.
type Replenishment struct {
	ID                   string `json:"id"`
	MatterID             string `json:"matter_id"`
	ClientID             string `json:"client_id"`
	AmountCents          int64  `json:"amount_cents"`
	AmountRequestedCents int64  `json:"amount_requested_cents"`
	Status               string `json:"status"`
}

// Amount is the requested amount from either read.
func (r Replenishment) Amount() int64 {
	if r.AmountCents > zero {
		return r.AmountCents
	}
	return r.AmountRequestedCents
}

// GetReplenishment reads a replenishment request.
func (c *Client) GetReplenishment(ctx context.Context, a Auth, id string) (Replenishment, error) {
	var r Replenishment
	err := c.Do(ctx, http.MethodGet, Path("/api/v1/trust-replenishment-requests", id), a, nil, &r)
	return r, err
}

// ClientAccess is a client's portal access (CORE-14).
type ClientAccess struct {
	UserID string `json:"user_id"`
	Active bool   `json:"active"`
}

// GetClientAccess reads a contact's portal access; a contact with none is 404.
func (c *Client) GetClientAccess(ctx context.Context, a Auth, contact string) (ClientAccess, error) {
	var ca ClientAccess
	err := c.Do(ctx, http.MethodGet, "/api/v1/client-access?"+url.Values{"contact_id": {contact}}.Encode(), a, nil, &ca)
	return ca, err
}

// File is a core file record.
type File struct {
	ID               string  `json:"id"`
	MatterID         *string `json:"matter_id"`
	Name             string  `json:"name"`
	Status           *string `json:"status"`
	SharedWithClient bool    `json:"shared_with_client"`
}

// StoreFile uploads data and publishes it as a file on the matter: createFileUpload,
// putFileChunk, completeFileUpload, publishFile. a is the viewer (the unsigned copy at Send)
// or a file publication grant (the signed copy from the webhook).
func (c *Client) StoreFile(ctx context.Context, a Auth, matter, name, mediaType string, data []byte, shared bool) (File, error) {
	sum := sha256.Sum256(data)
	var up struct {
		ID string `json:"id"`
	}
	key := a.Idempotency
	a.Idempotency = ""
	if err := c.Do(ctx, http.MethodPost, "/api/v1/file-uploads", a, map[string]any{"file_name": name, "matter_id": matter,
		"media_type": mediaType, "size_bytes": len(data), "sha256": hex.EncodeToString(sum[:])}, &up); err != nil {
		return File{}, err
	}
	rng := fmt.Sprintf("bytes 0-%d/%d", len(data)-1, len(data))
	if err := c.DoRaw(ctx, http.MethodPut, Path("/api/v1/file-uploads", up.ID), a, bytes.NewReader(data), "application/octet-stream",
		map[string]string{"Content-Range": rng}, nil); err != nil {
		return File{}, err
	}
	if err := c.Do(ctx, http.MethodPost, Path("/api/v1/file-uploads", up.ID, "complete"), a, nil, nil); err != nil {
		return File{}, err
	}
	a.Idempotency = key
	var f File
	err := c.Do(ctx, http.MethodPost, "/api/v1/files", a, map[string]any{"upload_id": up.ID, "matter_id": matter, "name": name,
		"shared_with_client": shared}, &f)
	return f, err
}

// FileGrant asks for a background publication grant on matter (delivered on
// file.background_authorized).
func (c *Client) FileGrant(ctx context.Context, matter string) error {
	return c.Do(ctx, http.MethodPost, "/api/v1/files/publication-grants", Service,
		map[string]any{"assignment": "matter", "matter_id": matter, "source": "importer"}, nil)
}

// FileStatus marks a file's status.
func (c *Client) FileStatus(ctx context.Context, a Auth, id, status string) (File, error) {
	var f File
	err := c.Do(ctx, http.MethodPost, Path("/api/v1/files", id, "status"), a, map[string]string{"status": status}, &f)
	return f, err
}

// GetFile reads a file.
func (c *Client) GetFile(ctx context.Context, a Auth, id string) (File, error) {
	var f File
	err := c.Do(ctx, http.MethodGet, Path("/api/v1/files", id), a, nil, &f)
	return f, err
}

// Event appends a timeline event.
func (c *Client) Event(ctx context.Context, a Auth, action, recordType, recordID, matter, summary string, clientVisible bool) error {
	return c.Do(ctx, http.MethodPost, "/api/v1/events", a, map[string]any{"action": action, "record_type": recordType,
		"record_id": recordID, "matter_id": matter, "summary": summary, "client_visible": clientVisible}, nil)
}

// Notify sends a notification to an audience on a matter (CORE-03). matter may be empty for
// the admins audience.
func (c *Client) Notify(ctx context.Context, a Auth, kind, title, link, audience, matter string) error {
	body := map[string]any{"kind": kind, "title": title, "link": link, "audience": audience}
	if matter != "" {
		body["matter_id"] = matter
	}
	return c.Do(ctx, http.MethodPost, "/api/v1/notifications", a, body, nil)
}

// MailResult is one recipient's result from module mail.
type MailResult struct {
	ContactID string `json:"contact_id"`
	UserID    string `json:"user_id"`
	Status    string `json:"status"`
	Error     string `json:"error"`
}

// Mail sends mail to parties on a matter through system mail (CORE-04). The core resolves
// the address and never returns it.
func (c *Client) Mail(ctx context.Context, a Auth, matter string, contacts []string, subject, text, link string) ([]MailResult, error) {
	body := map[string]any{"matter_id": matter, "to_contact_ids": contacts, "subject": subject, "text": text}
	if link != "" {
		body["link_path"] = link
	}
	var out struct {
		Results []MailResult `json:"results"`
	}
	err := c.Do(ctx, http.MethodPost, "/api/v1/mail/messages", a, body, &out)
	return out.Results, err
}

// ConflictCheck is a conflict check with its hits' decisions.
type ConflictCheck struct {
	ID       string `json:"id"`
	MatterID string `json:"matter_id"`
	Hits     []struct {
		ID       string  `json:"id"`
		Decision *string `json:"decision"`
	} `json:"hits"`
}

// NoGo reports whether any hit was decided no_go.
func (c ConflictCheck) NoGo() bool {
	for _, h := range c.Hits {
		if h.Decision != nil && *h.Decision == "no_go" {
			return true
		}
	}
	return false
}

// GetConflictCheck reads a conflict check.
func (c *Client) GetConflictCheck(ctx context.Context, a Auth, id string) (ConflictCheck, error) {
	var cc ConflictCheck
	err := c.Do(ctx, http.MethodGet, Path("/api/v1/conflict-checks", id), a, nil, &cc)
	return cc, err
}

// BooksMatter is the books reader's matter (CORE-02).
type BooksMatter struct {
	ID                        string  `json:"id"`
	Number                    string  `json:"number"`
	Name                      string  `json:"name"`
	Status                    string  `json:"status"`
	ClientContactID           string  `json:"client_contact_id"`
	ResponsibleAttorneyUserID *string `json:"responsible_attorney_user_id"`
}

// GetBooksMatter reads a matter as the books reader.
func (c *Client) GetBooksMatter(ctx context.Context, id string) (BooksMatter, error) {
	var m BooksMatter
	err := c.Do(ctx, http.MethodGet, Path("/api/v1/books/matters", id), Service, nil, &m)
	return m, err
}

// BooksProfile is the firm's books profile (CORE-02).
type BooksProfile struct {
	FirmName string `json:"firm_name"`
	TimeZone string `json:"time_zone"`
}

// GetBooksProfile reads it.
func (c *Client) GetBooksProfile(ctx context.Context) (BooksProfile, error) {
	var p BooksProfile
	err := c.Do(ctx, http.MethodGet, "/api/v1/books/profile", Service, nil, &p)
	return p, err
}

// Itoa formats an int64 for a query.
func Itoa(v int64) string { return strconv.FormatInt(v, decimalBase) }
