package lawpaydocuseal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/CharitonMedia/togavsu-lawpay-docuseal/adapters"
	"github.com/CharitonMedia/togavsu-lawpay-docuseal/internal/coreapi"
)

// Fee agreement limits (U11). These are the agreement's legal shape, not tunables.
const (
	maxAgreementCents = 99999999
	maxServicesRunes  = 4000
	maxCaseNumber     = 100
)

// AgreementRequest saves a fee agreement draft.
type AgreementRequest struct {
	ID                  string `json:"id"`
	MatterID            string `json:"matter_id"`
	FeeType             string `json:"fee_type"`
	AmountCents         *int64 `json:"amount_cents"`
	ServicesDescription string `json:"services_description"`
	CaseNumber          string `json:"case_number"`
	NewCase             *bool  `json:"new_case"`
}

// Agreement is a core fee agreement with the module's extension.
type Agreement struct {
	coreapi.FeeAgreement
	CaseNumber string `json:"case_number"`
	NewCase    *bool  `json:"new_case"`
}

var feeTypes = map[string]string{"flat": "Flat Fee", "hourly": "Advance Fee", "mixed": "Mixed Fee", "contingency": "Contingency Fee", "pro_bono": "Pro Bono"}

func closedMatter() *appError {
	return fail(http.StatusConflict, "matter_closed", "This matter is closed, so its fee agreement is read-only.")
}

// SaveDraft creates or changes a draft and fills an empty billing type (U12, NULL-only).
func (s *Server) SaveDraft(ctx context.Context, v viewer, req AgreementRequest) (Agreement, error) {
	if !v.canCharge() {
		return Agreement{}, errForbidden
	}
	label, ok := feeTypes[req.FeeType]
	_ = label
	if !ok {
		return Agreement{}, &appError{Status: http.StatusUnprocessableEntity, Code: "validation_failed", Message: "Pick a fee type.", Fields: []string{"fee_type"}}
	}
	needsAmount := req.FeeType != "contingency" && req.FeeType != "pro_bono"
	if (needsAmount && req.AmountCents == nil) || (req.AmountCents != nil && (*req.AmountCents <= 0 || *req.AmountCents > maxAgreementCents)) {
		return Agreement{}, &appError{Status: http.StatusUnprocessableEntity, Code: "validation_failed",
			Message: "Enter an amount above $0.00 and at most " + dollars(maxAgreementCents) + ".", Fields: []string{"amount_cents"}}
	}
	desc := strings.TrimSpace(req.ServicesDescription)
	if desc == "" || utf8.RuneCountInString(desc) > maxServicesRunes {
		return Agreement{}, &appError{Status: http.StatusUnprocessableEntity, Code: "validation_failed",
			Message: fmt.Sprintf("Describe the services in 1 to %d characters.", maxServicesRunes), Fields: []string{"services_description"}}
	}
	if utf8.RuneCountInString(req.CaseNumber) > maxCaseNumber {
		return Agreement{}, &appError{Status: http.StatusUnprocessableEntity, Code: "validation_failed",
			Message: fmt.Sprintf("A case number is at most %d characters.", maxCaseNumber), Fields: []string{"case_number"}}
	}
	matter := req.MatterID
	var existing coreapi.FeeAgreement
	if req.ID != "" {
		fa, err := s.core.GetFeeAgreement(ctx, v.auth(), req.ID)
		if err != nil {
			return Agreement{}, notFoundAs(err)
		}
		if fa.Status != "draft" {
			return Agreement{}, fail(http.StatusConflict, "not_draft", "Only a draft can be changed. Void it to start again.")
		}
		existing, matter = fa, fa.MatterID
	}
	m, err := s.core.GetMatter(ctx, v.auth(), matter)
	if err != nil {
		return Agreement{}, notFoundAs(err)
	}
	if m.Status == "closed" {
		return Agreement{}, closedMatter()
	}
	body := map[string]any{"fee_type": req.FeeType, "amount_cents": req.AmountCents, "services_description": desc}
	var fa coreapi.FeeAgreement
	if existing.ID == "" {
		body["matter_id"] = matter
		body["status"] = "draft"
		fa, err = s.core.CreateFeeAgreement(ctx, v.auth(), body)
	} else {
		fa, err = s.core.PatchFeeAgreement(ctx, v.auth(), existing.ID, body)
	}
	if err != nil {
		return Agreement{}, err
	}
	if _, err := s.c.DB.ExecContext(ctx, `INSERT INTO agreement_details (fee_agreement_id, case_number, new_case, updated_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (fee_agreement_id) DO UPDATE SET case_number = $2, new_case = $3, updated_at = $4`,
		fa.ID, req.CaseNumber, req.NewCase, s.now()); err != nil {
		return Agreement{}, err
	}
	patch := map[string]any{}
	billing := ""
	if m.BillingType != nil {
		billing = *m.BillingType
	} else {
		billing = req.FeeType
		patch["billing_type"] = billing
	}
	if billing == "flat" && req.FeeType == "flat" && m.FlatFeeCents == nil && req.AmountCents != nil {
		patch["flat_fee_cents"] = *req.AmountCents
	}
	if len(patch) > 0 {
		if _, err := s.core.PatchMatter(ctx, v.auth(), matter, patch); err != nil {
			return Agreement{}, err
		}
	}
	return Agreement{FeeAgreement: fa, CaseNumber: req.CaseNumber, NewCase: req.NewCase}, nil
}

func notFoundAs(err error) error {
	if coreapi.Is(err, http.StatusNotFound) || coreapi.Is(err, http.StatusForbidden) {
		return errNotFound
	}
	return err
}

func (s *Server) details(ctx context.Context, agreement string) (string, *bool, error) {
	var cn string
	var nc sql.NullBool
	err := s.c.DB.QueryRowContext(ctx, `SELECT case_number, new_case FROM agreement_details WHERE fee_agreement_id = $1`, agreement).Scan(&cn, &nc)
	if errors.Is(err, errNoRows) {
		return "", nil, nil
	}
	if err != nil || !nc.Valid {
		return cn, nil, err
	}
	b := nc.Bool
	return cn, &b, nil
}

// Signing is a signings row as the panel and API show it. The DocuSeal signing URL is
// never shown to staff and never leaves the module except on the client's own sign tab.
type Signing struct {
	ID                       string     `json:"id"`
	FeeAgreementID           string     `json:"fee_agreement_id"`
	MatterID                 string     `json:"matter_id"`
	ClientContactID          string     `json:"client_contact_id"`
	State                    string     `json:"state"`
	SubmissionID             string     `json:"docuseal_submission_id,omitempty"`
	SentAt                   *time.Time `json:"sent_at,omitempty"`
	ClientSignedAt           *time.Time `json:"client_signed_at,omitempty"`
	ClientSignedDuringVoidAt *time.Time `json:"client_signed_during_void_at,omitempty"`
	CountersignedAt          *time.Time `json:"countersigned_at,omitempty"`
	ExecutedAt               *time.Time `json:"executed_at,omitempty"`
	ExpiresAt                *time.Time `json:"expires_at,omitempty"`
	LegalExecutionDate       string     `json:"legal_execution_date,omitempty"`
	SentFileID               string     `json:"sent_file_id,omitempty"`
	SignedFileID             string     `json:"signed_file_id,omitempty"`
	VoidReason               string     `json:"void_reason,omitempty"`
	LastError                string     `json:"last_error,omitempty"`
	InviteError              string     `json:"invite_error,omitempty"`
	PDFAttempts              int        `json:"pdf_attempts"`
	Label                    string     `json:"label"`

	clientSubmitter, firmSubmitter, signingURL, priorState string
}

const signingCols = `id, fee_agreement_id, matter_id, client_contact_id, state, COALESCE(docuseal_submission_id, ''), sent_at, client_signed_at,
	client_signed_during_void_at, countersigned_at, executed_at, expires_at, COALESCE(to_char(legal_execution_date, 'YYYY-MM-DD'), ''),
	COALESCE(sent_file_id, ''), COALESCE(signed_file_id, ''), void_reason, last_error, invite_error, pdf_attempts, client_submitter_id,
	firm_submitter_id, client_signing_url, COALESCE(prior_state, '')`

func scanSigning(r scanner) (Signing, error) {
	var g Signing
	var sent, csigned, during, counter, exec, exp sql.NullTime
	err := r.Scan(&g.ID, &g.FeeAgreementID, &g.MatterID, &g.ClientContactID, &g.State, &g.SubmissionID, &sent, &csigned, &during, &counter,
		&exec, &exp, &g.LegalExecutionDate, &g.SentFileID, &g.SignedFileID, &g.VoidReason, &g.LastError, &g.InviteError, &g.PDFAttempts,
		&g.clientSubmitter, &g.firmSubmitter, &g.signingURL, &g.priorState)
	t := func(n sql.NullTime) *time.Time {
		if !n.Valid {
			return nil
		}
		v := n.Time.UTC()
		return &v
	}
	g.SentAt, g.ClientSignedAt, g.ClientSignedDuringVoidAt, g.CountersignedAt, g.ExecutedAt, g.ExpiresAt = t(sent), t(csigned), t(during), t(counter), t(exec), t(exp)
	g.Label = stateLabel(g.State)
	return g, err
}

func stateLabel(state string) string {
	switch state {
	case "sending":
		return "Sending"
	case "sent":
		return "Awaiting client signature"
	case "client_signed":
		return "Awaiting countersignature"
	case "processing_post_execution", "executed":
		return "Executed"
	case "declined":
		return "Declined by client"
	case "expired":
		return "Expired"
	case "voiding":
		return "Voiding"
	case "voided":
		return "Voided"
	case "error":
		return "Error"
	}
	return state
}

func (s *Server) signingBy(ctx context.Context, col, val string) (Signing, bool, error) {
	g, err := scanSigning(s.c.DB.QueryRowContext(ctx, `SELECT `+signingCols+` FROM signings WHERE `+col+` = $1`, val))
	if errors.Is(err, errNoRows) {
		return g, false, nil
	}
	return g, err == nil, err
}

func (s *Server) signings(ctx context.Context, where string, args ...any) ([]Signing, error) {
	rows, err := s.c.DB.QueryContext(ctx, `SELECT `+signingCols+` FROM signings WHERE `+where+` ORDER BY created_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Signing
	for rows.Next() {
		g, err := scanSigning(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Server) setSigning(ctx context.Context, id, set string, args ...any) error {
	_, err := s.c.DB.ExecContext(ctx, `UPDATE signings SET `+set+`, updated_at = $2 WHERE id = $1`, append([]any{id, s.now()}, args...)...)
	return err
}

var (
	stateRe = regexp.MustCompile(`^[A-Za-z]{2}$`)
	zipRe   = regexp.MustCompile(`^\d{5}(-\d{4})?$`)
	emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
)

// sendChecks lists the client fields DocuSeal needs that are missing or malformed.
func sendChecks(d coreapi.Details) []string {
	var bad []string
	if !emailRe.MatchString(strings.TrimSpace(d.Email)) {
		bad = append(bad, "email")
	}
	if strings.TrimSpace(d.Phone) == "" {
		bad = append(bad, "phone")
	}
	if strings.TrimSpace(d.Address.Line1) == "" {
		bad = append(bad, "street")
	}
	if strings.TrimSpace(d.Address.City) == "" {
		bad = append(bad, "city")
	}
	if !stateRe.MatchString(strings.TrimSpace(d.Address.Region)) {
		bad = append(bad, "state")
	}
	if !zipRe.MatchString(strings.TrimSpace(d.Address.PostalCode)) {
		bad = append(bad, "zip")
	}
	return bad
}

func isUnique(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

func signLink(id string) string { return "/portal/" + ID + "/sign?signing_request_id=" + id }

// Send sends a draft for signature. Every check runs before any write; a failure after
// the submission exists archives it and leaves the agreement a draft.
func (s *Server) Send(ctx context.Context, v viewer, agreementID string) (Signing, error) {
	st, err := s.settings(ctx)
	if err != nil {
		return Signing{}, err
	}
	if !st.DocuSealReady() {
		return Signing{}, errNoDocuSeal
	}
	if !v.canCharge() {
		return Signing{}, errForbidden
	}
	fa, err := s.core.GetFeeAgreement(ctx, v.auth(), agreementID)
	if err != nil {
		return Signing{}, notFoundAs(err)
	}
	if fa.Status != "draft" {
		return Signing{}, fail(http.StatusConflict, "not_draft", "Only a draft can be sent.")
	}
	m, err := s.core.GetMatter(ctx, v.auth(), fa.MatterID)
	if err != nil {
		return Signing{}, notFoundAs(err)
	}
	if m.Status == "closed" {
		return Signing{}, closedMatter()
	}
	client, err := s.core.ClientParty(ctx, v.auth(), fa.MatterID)
	if err != nil {
		return Signing{}, err
	}
	if client == "" {
		return Signing{}, fail(http.StatusUnprocessableEntity, "validation_failed", "Add the client to the matter first.")
	}
	contact, err := s.core.GetContact(ctx, v.auth(), client)
	if err != nil {
		return Signing{}, err
	}
	d, err := s.core.ContactDetails(ctx, v.auth(), client)
	if err != nil {
		return Signing{}, err
	}
	if bad := sendChecks(d); len(bad) > 0 {
		return Signing{}, &appError{Status: http.StatusUnprocessableEntity, Code: "validation_failed", Fields: bad,
			Message: "Fix the client's " + strings.Join(bad, ", ") + " before sending."}
	}
	access, err := s.core.GetClientAccess(ctx, v.auth(), client)
	if (err != nil && coreapi.Is(err, http.StatusNotFound)) || (err == nil && !access.Active) {
		return Signing{}, fail(http.StatusUnprocessableEntity, "no_portal_access", "Give the client portal access first; they sign in the portal.")
	}
	if err != nil {
		return Signing{}, err
	}
	caseNumber, newCase, err := s.details(ctx, fa.ID)
	if err != nil {
		return Signing{}, err
	}
	if caseNumber == "" {
		caseNumber = m.CaseNumber
	}
	id := s.id()
	if _, err := s.c.DB.ExecContext(ctx, `INSERT INTO signings (id, fee_agreement_id, matter_id, client_contact_id, state, last_action_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'sending', $5, $6, $6)`, id, fa.ID, fa.MatterID, client, v.UserID, s.now()); err != nil {
		if isUnique(err) {
			return Signing{}, fail(http.StatusConflict, "already_sending", "This fee agreement is already being sent.")
		}
		return Signing{}, err
	}
	bg := context.WithoutCancel(ctx)
	ds := s.docuseal(st)
	var expireAt time.Time
	if st.ExpireDays > 0 {
		expireAt = s.now().AddDate(0, 0, st.ExpireDays)
	}
	amount := ""
	if fa.AmountCents != nil {
		amount = dollars(*fa.AmountCents)
	}
	newCaseText := ""
	if newCase != nil {
		newCaseText = map[bool]string{true: "Yes", false: "No"}[*newCase]
	}
	addr := strings.TrimSpace(d.Address.Line1 + " " + d.Address.Line2)
	sub, err := ds.CreateSubmission(bg, adapters.SubmissionRequest{TemplateID: st.TemplateID, ExpireAt: expireAt, Submitters: []adapters.SubmitterRequest{
		{Role: adapters.RoleClient, Name: contact.Name(), Email: d.Email, SendEmail: false, Fields: []adapters.Field{
			{Name: "clientName", Value: contact.Name(), ReadOnly: true},
			{Name: "clientAddress", Value: addr, ReadOnly: true},
			{Name: "clientCityStateZip", Value: fmt.Sprintf("%s, %s %s", d.Address.City, strings.ToUpper(d.Address.Region), d.Address.PostalCode), ReadOnly: true},
			{Name: "clientPhone", Value: d.Phone, ReadOnly: true},
			{Name: "clientEmail", Value: d.Email, ReadOnly: true},
		}},
		{Role: adapters.RoleFirm, Name: st.SignerName, Email: st.SignerEmail, SendEmail: true, Fields: []adapters.Field{
			{Name: "representationDescription", Value: fa.ServicesDescription},
			{Name: "caseNumber", Value: caseNumber},
			{Name: "newCase", Value: newCaseText},
			{Name: "feeType", Value: feeTypes[fa.FeeType]},
			{Name: "feeAmount", Value: amount},
		}},
	}})
	if err != nil {
		_ = s.setSigning(bg, id, `state = 'error', last_error = $3`, "DocuSeal refused the submission: "+docusealMessage(err))
		return Signing{}, fail(http.StatusBadGateway, "docuseal_failed", "DocuSeal couldn't create the signing: "+docusealMessage(err))
	}
	clientSub, _ := sub.Submitter(adapters.RoleClient)
	firmSub, _ := sub.Submitter(adapters.RoleFirm)
	undo := func(cause string) (Signing, error) {
		if aerr := ds.Archive(bg, sub.ID); aerr != nil {
			_ = s.setSigning(bg, id, `state = 'error', docuseal_submission_id = NULL, last_error = $3`,
				"Orphan DocuSeal submission "+sub.ID+" could not be archived after: "+cause)
			s.openException(bg, exception{Kind: "docuseal_orphan", MatterID: fa.MatterID, SigningID: id, TransactionID: sub.ID,
				Detail: "A DocuSeal submission was left behind after a failed send. Archive it in DocuSeal."})
			return Signing{}, fail(http.StatusBadGateway, "send_failed", "The fee agreement wasn't sent. "+cause)
		}
		_, _ = s.c.DB.ExecContext(bg, `DELETE FROM signings WHERE id = $1`, id)
		return Signing{}, fail(http.StatusBadGateway, "send_failed", "The fee agreement wasn't sent and is still a draft. "+cause)
	}
	storeIDs := func() error {
		if s.failStoreIDs != nil {
			if err := s.failStoreIDs(); err != nil {
				return err
			}
		}
		return s.setSigning(bg, id, `docuseal_submission_id = $3, client_submitter_id = $4, firm_submitter_id = $5, client_signing_url = $6,
			audit_url = $7`, sub.ID, clientSub.ID, firmSub.ID, clientSub.SigningURL, sub.AuditURL)
	}
	var serr error
	for attempt := 0; attempt <= st.RetryLimit; attempt++ {
		if serr = storeIDs(); serr == nil {
			break
		}
	}
	if serr != nil {
		return undo("The signing couldn't be saved.")
	}
	pdf, err := ds.CurrentPDF(bg, sub.ID)
	var file coreapi.File
	if err == nil && s.failFileSent != nil {
		err = s.failFileSent()
	}
	if err == nil {
		a := v.auth()
		a.Idempotency = "lpds-sent-" + id
		file, err = s.core.StoreFile(bg, a, fa.MatterID, "Fee agreement sent for signature.pdf", "application/pdf", pdf, true)
	}
	if err != nil {
		return undo("The unsigned copy couldn't be filed on the matter.")
	}
	if err := s.setSigning(bg, id, `sent_file_id = $3`, file.ID); err != nil {
		return undo("The unsigned copy couldn't be linked.")
	}
	if _, err := s.core.PatchFeeAgreement(bg, v.auth(), fa.ID, map[string]any{"status": "sent"}); err != nil {
		_ = s.setSigning(bg, id, `sent_file_id = NULL`)
		return undo("Togavsu didn't accept the sent status.")
	}
	var exp any
	if !expireAt.IsZero() {
		exp = expireAt
	}
	if err := s.setSigning(bg, id, `state = 'sent', sent_at = $3, expires_at = $4`, s.now(), exp); err != nil {
		return Signing{}, err
	}
	s.invite(bg, v, id, fa.MatterID, client, "Your fee agreement is ready to sign",
		"Your fee agreement is ready. Sign in to the client portal to review and sign it.", true)
	s.event(bg, v.auth(), "fee_agreement.sent_for_signature", "fee_agreement", fa.ID, fa.MatterID, "Fee agreement sent for signature", true)
	g, _, err := s.signingBy(bg, "id", id)
	return g, err
}

func docusealMessage(err error) string {
	var de *adapters.DocuSealError
	if errors.As(err, &de) && de.Message != "" {
		return de.Message
	}
	return "no answer from DocuSeal"
}

// invite mails the client through core mail and records the result on the signing.
func (s *Server) invite(ctx context.Context, v viewer, signing, matter, client, subject, text string, link bool) bool {
	lp := ""
	if link {
		lp = signLink(signing)
	}
	res, err := s.core.Mail(ctx, v.auth(), matter, []string{client}, subject, text, lp)
	msg := ""
	switch {
	case err != nil:
		msg = "The email didn't go out. Use Resend signing link."
	case len(res) == 0 || res[0].Status != "sent":
		msg = "The email didn't go out. Use Resend signing link."
		if len(res) > 0 && res[0].Error != "" {
			msg = "The email didn't go out (" + res[0].Error + "). Use Resend signing link."
		}
	}
	if link {
		_ = s.setSigning(ctx, signing, `invite_error = $3`, msg)
	}
	return msg == ""
}

// Resend mails the same signing link again; no new submission.
func (s *Server) Resend(ctx context.Context, v viewer, id string) (Signing, error) {
	if !v.canCharge() {
		return Signing{}, errForbidden
	}
	g, found, err := s.signingBy(ctx, "id", id)
	if err != nil {
		return Signing{}, err
	}
	if !found || !s.canSee(ctx, v, g.MatterID) {
		return Signing{}, errNotFound
	}
	if g.State != "sent" {
		return Signing{}, fail(http.StatusConflict, "not_sent", "Only an agreement waiting for the client can be resent.")
	}
	if !s.invite(ctx, v, g.ID, g.MatterID, g.ClientContactID, "Reminder: your fee agreement is ready to sign",
		"Your fee agreement is still waiting for your signature. Sign in to the client portal to review and sign it.", true) {
		g, _, _ = s.signingBy(ctx, "id", id)
		return g, fail(http.StatusBadGateway, "mail_failed", g.InviteError)
	}
	g, _, err = s.signingBy(ctx, "id", id)
	return g, err
}

// Void archives the submission and voids the core agreement with the reason.
func (s *Server) Void(ctx context.Context, v viewer, id, reason string) (Signing, error) {
	st, err := s.settings(ctx)
	if err != nil {
		return Signing{}, err
	}
	if !st.DocuSealReady() {
		return Signing{}, errNoDocuSeal
	}
	if !v.canCharge() {
		return Signing{}, errForbidden
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Signing{}, &appError{Status: http.StatusUnprocessableEntity, Code: "validation_failed", Message: "Give a reason.", Fields: []string{"reason"}}
	}
	g, found, err := s.signingBy(ctx, "id", id)
	if err != nil {
		return Signing{}, err
	}
	if !found || !s.canSee(ctx, v, g.MatterID) {
		return Signing{}, errNotFound
	}
	res, err := s.c.DB.ExecContext(ctx, `UPDATE signings SET state = 'voiding', prior_state = state, void_reason = $2, last_action_by = $3, updated_at = $4
		WHERE id = $1 AND state IN ('sent', 'client_signed')`, id, reason, v.UserID, s.now())
	if err != nil {
		return Signing{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Signing{}, fail(http.StatusConflict, "not_voidable", "Only a sent agreement can be voided.")
	}
	bg := context.WithoutCancel(ctx)
	if s.duringVoid != nil {
		s.duringVoid()
	}
	if err := s.docuseal(st).Archive(bg, g.SubmissionID); err != nil && !adapters.DocuSealNotFound(err) {
		_ = s.setSigning(bg, id, `state = prior_state, last_error = $3`, "DocuSeal couldn't archive the submission: "+docusealMessage(err))
		return Signing{}, fail(http.StatusBadGateway, "docuseal_failed", "DocuSeal couldn't void the signing. Try again.")
	}
	if _, err := s.core.PatchFeeAgreement(bg, v.auth(), g.FeeAgreementID, map[string]any{"status": "void", "void_reason": reason}); err != nil {
		_ = s.setSigning(bg, id, `last_error = $3`, "Togavsu didn't accept the void: "+asAppError(err).Message)
		return Signing{}, err
	}
	if err := s.setSigning(bg, id, `state = 'voided'`); err != nil {
		return Signing{}, err
	}
	s.invite(bg, v, id, g.MatterID, g.ClientContactID, "Your fee agreement was withdrawn",
		"The fee agreement we sent you was withdrawn, so you don't need to sign it. We'll be in touch.", false)
	g, _, err = s.signingBy(bg, "id", id)
	return g, err
}

// canSee reports whether the viewer can read the matter (a walled matter answers 404).
func (s *Server) canSee(ctx context.Context, v viewer, matter string) bool {
	_, err := s.core.GetMatter(ctx, v.auth(), matter)
	return err == nil
}
