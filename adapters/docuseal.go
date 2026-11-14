package adapters

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DocuSeal is the DocuSeal API as the module uses it (Unified's src/lib/docuseal-client.ts).
type DocuSeal interface {
	CreateSubmission(ctx context.Context, r SubmissionRequest) (Submission, error)
	Submission(ctx context.Context, id string) (Submission, error)
	CurrentPDF(ctx context.Context, id string) ([]byte, error) // the agreement as sent, before signing
	CompletedPDF(ctx context.Context, id string) ([]byte, error)
	Archive(ctx context.Context, id string) error
	// Template reads the fee agreement template; Test connection uses it.
	Template(ctx context.Context, id int64) error
}

// Submitter roles on the firm's fee agreement template.
const (
	RoleClient = "Client"
	RoleFirm   = "Firm"
)

// SubmissionRequest creates one submission from the template. ExpireAt zero means none.
type SubmissionRequest struct {
	TemplateID int64
	ExpireAt   time.Time
	Submitters []SubmitterRequest
}

// SubmitterRequest is one signer and the fields filled for them.
type SubmitterRequest struct {
	Role, Name, Email string
	SendEmail         bool
	Fields            []Field
}

// Field is one prefilled template field.
type Field struct {
	Name, Value string
	ReadOnly    bool
}

// Submission is a DocuSeal submission.
type Submission struct {
	ID                  string
	Status              string
	Submitters          []Submitter
	AuditURL            string
	CombinedDocumentURL string
	ExpireAt            time.Time
}

// Submitter is one signer on a submission. SigningURL is DocuSeal's signing page for them.
type Submitter struct {
	ID, Role, Status, SigningURL string
}

// Completed reports whether DocuSeal shows the submission completed: its own status, or
// every submitter completed.
func (s Submission) Completed() bool {
	if strings.EqualFold(s.Status, "completed") {
		return true
	}
	if len(s.Submitters) == 0 {
		return false
	}
	for _, sub := range s.Submitters {
		if !strings.EqualFold(sub.Status, "completed") {
			return false
		}
	}
	return true
}

// Submitter returns the submitter with role.
func (s Submission) Submitter(role string) (Submitter, bool) {
	for _, sub := range s.Submitters {
		if strings.EqualFold(sub.Role, role) {
			return sub, true
		}
	}
	return Submitter{}, false
}

// DocuSealError is a non-2xx DocuSeal answer.
type DocuSealError struct {
	HTTPStatus int
	Message    string
}

func (e *DocuSealError) Error() string {
	return fmt.Sprintf("docuseal %d: %s", e.HTTPStatus, e.Message)
}

// DocuSealNotFound reports whether err is DocuSeal's 404.
func DocuSealNotFound(err error) bool {
	var de *DocuSealError
	return errors.As(err, &de) && de.HTTPStatus == http.StatusNotFound
}

// DocuSealHTTP is the live adapter: X-Auth-Token on every call, base docuseal_base_url.
type DocuSealHTTP struct {
	Base, APIKey string
	HTTP         *http.Client
}

// NewDocuSealHTTP builds the live adapter from settings.
func NewDocuSealHTTP(base, key string, c *http.Client) *DocuSealHTTP {
	if c == nil {
		c = http.DefaultClient
	}
	return &DocuSealHTTP{Base: strings.TrimRight(base, "/"), APIKey: key, HTTP: c}
}

func (d *DocuSealHTTP) raw(ctx context.Context, method, target string, body any) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	if strings.HasPrefix(target, "/") {
		target = d.Base + target
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return nil, err
	}
	// The API key goes only to DocuSeal's own host, never to a document URL elsewhere.
	if base, err := url.Parse(d.Base); err == nil && base.Host == req.URL.Host {
		req.Header.Set("X-Auth-Token", d.APIKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		msg := resp.Status
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(out, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return nil, &DocuSealError{HTTPStatus: resp.StatusCode, Message: msg}
	}
	return out, nil
}

type dsSubmitter struct {
	ID           json.Number `json:"id"`
	SubmissionID json.Number `json:"submission_id"`
	Role         string      `json:"role"`
	Status       string      `json:"status"`
	EmbedSrc     string      `json:"embed_src"`
	Slug         string      `json:"slug"`
}

type dsSubmission struct {
	ID                  json.Number   `json:"id"`
	Status              string        `json:"status"`
	Submitters          []dsSubmitter `json:"submitters"`
	AuditLogURL         string        `json:"audit_log_url"`
	CombinedDocumentURL string        `json:"combined_document_url"`
	ExpireAt            string        `json:"expire_at"`
	Documents           []struct {
		URL string `json:"url"`
	} `json:"documents"`
}

func (s dsSubmission) submission() Submission {
	out := Submission{ID: s.ID.String(), Status: s.Status, AuditURL: s.AuditLogURL, CombinedDocumentURL: s.CombinedDocumentURL}
	if t, err := time.Parse(time.RFC3339, s.ExpireAt); err == nil {
		out.ExpireAt = t
	}
	for _, sub := range s.Submitters {
		out.Submitters = append(out.Submitters, Submitter{ID: sub.ID.String(), Role: sub.Role, Status: sub.Status, SigningURL: sub.EmbedSrc})
	}
	return out
}

// CreateSubmission creates the submission. DocuSeal answers either an array of submitters
// or an object with submitters; both shapes are read.
func (d *DocuSealHTTP) CreateSubmission(ctx context.Context, r SubmissionRequest) (Submission, error) {
	type field struct {
		Name     string `json:"name"`
		Value    string `json:"default_value"`
		ReadOnly bool   `json:"readonly"`
	}
	type submitter struct {
		Role      string  `json:"role"`
		Name      string  `json:"name"`
		Email     string  `json:"email"`
		SendEmail bool    `json:"send_email"`
		Fields    []field `json:"fields"`
	}
	body := map[string]any{"template_id": r.TemplateID, "send_email": false}
	if !r.ExpireAt.IsZero() {
		body["expire_at"] = r.ExpireAt.UTC().Format(time.RFC3339)
	}
	var subs []submitter
	for _, s := range r.Submitters {
		sub := submitter{Role: s.Role, Name: s.Name, Email: s.Email, SendEmail: s.SendEmail}
		for _, f := range s.Fields {
			sub.Fields = append(sub.Fields, field{Name: f.Name, Value: f.Value, ReadOnly: f.ReadOnly})
		}
		subs = append(subs, sub)
	}
	body["submitters"] = subs
	raw, err := d.raw(ctx, http.MethodPost, "/api/submissions", body)
	if err != nil {
		return Submission{}, err
	}
	return parseCreated(raw)
}

func parseCreated(raw []byte) (Submission, error) {
	var list []dsSubmitter
	if err := json.Unmarshal(raw, &list); err == nil {
		if len(list) == 0 {
			return Submission{}, errors.New("docuseal: the submission came back with no submitters")
		}
		s := dsSubmission{ID: list[0].SubmissionID, Submitters: list}
		return s.submission(), nil
	}
	var obj dsSubmission
	if err := json.Unmarshal(raw, &obj); err != nil {
		return Submission{}, err
	}
	if obj.ID == "" && len(obj.Submitters) > 0 {
		obj.ID = obj.Submitters[0].SubmissionID
	}
	return obj.submission(), nil
}

// Submission reads one submission.
func (d *DocuSealHTTP) Submission(ctx context.Context, id string) (Submission, error) {
	raw, err := d.raw(ctx, http.MethodGet, "/api/submissions/"+url.PathEscape(id), nil)
	if err != nil {
		return Submission{}, err
	}
	var s dsSubmission
	if err := json.Unmarshal(raw, &s); err != nil {
		return Submission{}, err
	}
	return s.submission(), nil
}

// CurrentPDF downloads the first document of the submission as it stands.
func (d *DocuSealHTTP) CurrentPDF(ctx context.Context, id string) ([]byte, error) {
	raw, err := d.raw(ctx, http.MethodGet, "/api/submissions/"+url.PathEscape(id)+"/documents", nil)
	if err != nil {
		return nil, err
	}
	var s dsSubmission
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	if len(s.Documents) == 0 || s.Documents[0].URL == "" {
		return nil, errors.New("docuseal: the submission has no document")
	}
	return d.raw(ctx, http.MethodGet, s.Documents[0].URL, nil)
}

// CompletedPDF downloads the combined document, else the first document.
func (d *DocuSealHTTP) CompletedPDF(ctx context.Context, id string) ([]byte, error) {
	raw, err := d.raw(ctx, http.MethodGet, "/api/submissions/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	var s dsSubmission
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	target := s.CombinedDocumentURL
	if target == "" && len(s.Documents) > 0 {
		target = s.Documents[0].URL
	}
	if target == "" {
		return nil, errors.New("docuseal: the submission has no completed document")
	}
	return d.raw(ctx, http.MethodGet, target, nil)
}

// Archive archives the submission.
func (d *DocuSealHTTP) Archive(ctx context.Context, id string) error {
	_, err := d.raw(ctx, http.MethodDelete, "/api/submissions/"+url.PathEscape(id), nil)
	return err
}

// Template reads the template.
func (d *DocuSealHTTP) Template(ctx context.Context, id int64) error {
	_, err := d.raw(ctx, http.MethodGet, "/api/templates/"+strconv.FormatInt(id, 10), nil)
	return err
}
