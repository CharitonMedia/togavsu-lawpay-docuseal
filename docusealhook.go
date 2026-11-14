package lawpaydocuseal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/CharitonMedia/togavsu-lawpay-docuseal/adapters"
	"github.com/CharitonMedia/togavsu-lawpay-docuseal/internal/coreapi"
)

// DocuSealEvent is the part of a DocuSeal webhook body the module reads.
type DocuSealEvent struct {
	Type         string
	Timestamp    string
	SubmissionID string
	SubmitterID  string
	Role         string
}

// ParseDocuSeal reads a DocuSeal webhook body. Form events carry the submitter in data and
// the submission in data.submission_id or data.submission.id; submission events carry the
// submission in data.id.
func ParseDocuSeal(body []byte) (DocuSealEvent, error) {
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return DocuSealEvent{}, errors.New("lawpay-docuseal: DocuSeal body is not a JSON object")
	}
	ev := DocuSealEvent{Type: str(m, "event_type"), Timestamp: str(m, "timestamp")}
	if strings.HasPrefix(ev.Type, "form.") {
		ev.SubmitterID = str(m, "data", "id")
		ev.Role = str(m, "data", "role")
		ev.SubmissionID = first(m, []string{"data", "submission_id"}, []string{"data", "submission", "id"})
	} else {
		ev.SubmissionID = first(m, []string{"data", "id"}, []string{"data", "submission_id"})
	}
	return ev, nil
}

// Key is the dedupe key: submission, event, submitter and time.
func (e DocuSealEvent) Key() string {
	return DedupeKey([]byte(e.SubmissionID + "|" + e.Type + "|" + e.SubmitterID + "|" + e.Timestamp))
}

var terminal = map[string]bool{"executed": true, "voided": true, "error": true, "declined": true, "expired": true}

// docusealHook receives DocuSeal webhooks through the core's inbound hook route.
func (s *Server) docusealHook(w http.ResponseWriter, r *http.Request) {
	st, err := s.settings(r.Context())
	if err != nil {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	if !tokenEqual(r.Header.Get("X-Webhook-Token"), st.DocuSealHookToken) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "unauthenticated"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	ev, err := ParseDocuSeal(body)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "verdict": "ignored"})
		return
	}
	ctx := r.Context()
	id := s.id()
	res, err := s.c.DB.ExecContext(ctx, `INSERT INTO docuseal_webhook_receipts (id, dedupe_key, submission_id, event_type, payload, verdict, received_at)
		VALUES ($1, $2, $3, $4, $5, 'received', $6) ON CONFLICT (dedupe_key) DO NOTHING`, id, ev.Key(), ev.SubmissionID, ev.Type, string(body), s.now())
	if err != nil {
		http.Error(w, "not stored", http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == noRows {
		_, _ = s.c.DB.ExecContext(ctx, `UPDATE docuseal_webhook_receipts SET duplicate_count = duplicate_count + 1 WHERE dedupe_key = $1`, ev.Key())
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "verdict": "duplicate"})
		return
	}
	verdict, signing, err := s.applyDocuSeal(ctx, st, ev)
	if err != nil {
		s.c.Logf("lawpay-docuseal: DocuSeal %s for submission %s failed: %v", ev.Type, ev.SubmissionID, err)
		_, _ = s.c.DB.ExecContext(context.WithoutCancel(ctx), `DELETE FROM docuseal_webhook_receipts WHERE id = $1`, id)
		http.Error(w, "not handled", http.StatusInternalServerError)
		return
	}
	_, _ = s.c.DB.ExecContext(ctx, `UPDATE docuseal_webhook_receipts SET verdict = $2, signing_id = $3 WHERE id = $1`, id, verdict, nullable(signing))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "verdict": verdict})
}

func (s *Server) applyDocuSeal(ctx context.Context, st Settings, ev DocuSealEvent) (string, string, error) {
	if ev.SubmissionID == "" {
		return "ignored", "", nil
	}
	g, found, err := s.signingBy(ctx, "docuseal_submission_id", ev.SubmissionID)
	if err != nil {
		return "", "", err
	}
	if !found {
		return "orphan", "", nil
	}
	if terminal[g.State] {
		return "terminal", g.ID, nil
	}
	isClient := strings.EqualFold(ev.Role, adapters.RoleClient) || (ev.SubmitterID != "" && ev.SubmitterID == g.clientSubmitter)
	isFirm := strings.EqualFold(ev.Role, adapters.RoleFirm) || (ev.SubmitterID != "" && ev.SubmitterID == g.firmSubmitter)
	at := s.now()
	if t, err := time.Parse(time.RFC3339Nano, ev.Timestamp); err == nil {
		at = t.UTC()
	}
	switch ev.Type {
	case "form.completed":
		switch {
		case isClient && g.State == "voiding":
			return "applied", g.ID, s.setSigning(ctx, g.ID, `client_signed_during_void_at = $3`, at)
		case isClient && g.State == "sent":
			res, err := s.c.DB.ExecContext(ctx, `UPDATE signings SET state = 'client_signed', client_signed_at = $2, last_action_by = 'webhook',
				updated_at = $3 WHERE id = $1 AND state = 'sent'`, g.ID, at, s.now())
			if err != nil {
				return "", "", err
			}
			if n, _ := res.RowsAffected(); n == oneRow {
				s.event(ctx, coreapi.Service, "fee_agreement.client_signed", "fee_agreement", g.FeeAgreementID, g.MatterID, "Client signed the fee agreement", true)
			}
			return "applied", g.ID, nil
		case isFirm:
			return "applied", g.ID, s.setSigning(ctx, g.ID, `countersigned_at = COALESCE(countersigned_at, $3)`, at)
		}
		return "ignored", g.ID, nil
	case "form.declined":
		if !isClient || (g.State != "sent" && g.State != "client_signed") {
			return "ignored", g.ID, nil
		}
		res, err := s.c.DB.ExecContext(ctx, `UPDATE signings SET state = 'declined', declined_at = $2, last_action_by = 'webhook', updated_at = $3
			WHERE id = $1 AND state IN ('sent', 'client_signed')`, g.ID, at, s.now())
		if err != nil {
			return "", "", err
		}
		if n, _ := res.RowsAffected(); n == oneRow {
			s.openException(ctx, exception{Kind: "signing_error", MatterID: g.MatterID, SigningID: g.ID, TransactionID: g.SubmissionID,
				Detail: "The client declined to sign the fee agreement."})
		}
		return "applied", g.ID, nil
	case "submission.expired":
		sub, err := s.docuseal(st).Submission(ctx, g.SubmissionID)
		if err != nil {
			return "", "", err
		}
		if !strings.EqualFold(sub.Status, "expired") {
			return "ignored", g.ID, nil
		}
		_, err = s.c.DB.ExecContext(ctx, `UPDATE signings SET state = 'expired', last_action_by = 'webhook', updated_at = $2
			WHERE id = $1 AND state IN ('sent', 'client_signed')`, g.ID, s.now())
		return "applied", g.ID, err
	case "submission.completed":
		sub, err := s.docuseal(st).Submission(ctx, g.SubmissionID)
		if err != nil {
			return "", "", err
		}
		if !sub.Completed() {
			return "ignored", g.ID, nil
		}
		res, err := s.c.DB.ExecContext(ctx, `UPDATE signings SET state = 'processing_post_execution', last_action_by = 'webhook', updated_at = $2
			WHERE id = $1 AND state IN ('sent', 'client_signed')`, g.ID, s.now())
		if err != nil {
			return "", "", err
		}
		if n, _ := res.RowsAffected(); n == noRows {
			return "ignored", g.ID, nil
		}
		return "applied", g.ID, s.execute(ctx, st, g, at)
	}
	return "ignored", g.ID, nil
}

// execute stamps the execution and moves to executed, then stores the signed PDF. A PDF
// failure keeps the signing executed; the sweep retries it.
func (s *Server) execute(ctx context.Context, st Settings, g Signing, at time.Time) error {
	loc := time.UTC
	if p, err := s.core.GetBooksProfile(ctx); err == nil && p.TimeZone != "" {
		if l, err := time.LoadLocation(p.TimeZone); err == nil {
			loc = l
		}
	}
	date := at.In(loc).Format(time.DateOnly)
	if err := s.setSigning(ctx, g.ID, `state = 'executed', executed_at = $3, legal_execution_date = $4, countersigned_at = COALESCE(countersigned_at, $3),
		client_signed_at = COALESCE(client_signed_at, $3)`, at, date); err != nil {
		return err
	}
	g.State, g.LegalExecutionDate = "executed", date
	if err := s.storeSigned(ctx, st, g); err != nil {
		s.c.Logf("lawpay-docuseal: signed PDF for signing %s deferred: %v", g.ID, err)
	}
	return nil
}

var pdfLocks sync.Map

// storeSigned files the executed PDF as a core file on the matter with a publication
// grant, marks it executed, moves the core agreement to signed and adds the timeline event.
func (s *Server) storeSigned(ctx context.Context, st Settings, g Signing) error {
	lock, _ := pdfLocks.LoadOrStore(g.ID, &sync.Mutex{})
	mu := lock.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	cur, _, err := s.signingBy(ctx, "id", g.ID)
	if err != nil {
		return err
	}
	if cur.SignedFileID != "" {
		return nil
	}
	err = s.fileSigned(ctx, st, cur)
	if err == nil {
		return nil
	}
	_ = s.setSigning(context.WithoutCancel(ctx), g.ID, `pdf_attempts = pdf_attempts + 1, last_error = $3`, "The signed PDF wasn't stored: "+err.Error())
	if cur.PDFAttempts >= st.RetryLimit {
		s.openException(context.WithoutCancel(ctx), exception{Kind: "signed_pdf_failed", MatterID: g.MatterID, SigningID: g.ID, TransactionID: g.SubmissionID,
			Detail: "The executed fee agreement couldn't be downloaded from DocuSeal and stored on the matter."})
	}
	return err
}

func (s *Server) fileSigned(ctx context.Context, st Settings, g Signing) error {
	pdf, err := s.docuseal(st).CompletedPDF(ctx, g.SubmissionID)
	if err != nil {
		return errors.New("DocuSeal download: " + docusealMessage(err))
	}
	cred, err := s.fileGrant(ctx, st, g.MatterID)
	if err != nil {
		return err
	}
	f, err := s.core.StoreFile(ctx, coreapi.Auth{Task: cred, Idempotency: "lpds-signed-" + g.ID}, g.MatterID, "Fee agreement (executed).pdf",
		"application/pdf", pdf, true)
	if err != nil {
		s.mu.Lock()
		delete(s.fileGrants, g.MatterID)
		s.mu.Unlock()
		return err
	}
	if err := s.setSigning(ctx, g.ID, `signed_file_id = $3, last_error = ''`, f.ID); err != nil {
		return err
	}
	if _, err := s.core.FileStatus(ctx, coreapi.Service, f.ID, "executed"); err != nil {
		s.c.Logf("lawpay-docuseal: file %s not marked executed: %v", f.ID, err)
	}
	if _, err := s.core.PatchFeeAgreement(ctx, coreapi.Service, g.FeeAgreementID, map[string]any{"status": "signed",
		"signed_on": g.LegalExecutionDate, "signed_file_id": f.ID}); err != nil {
		return err
	}
	s.event(ctx, coreapi.Service, "fee_agreement.executed", "fee_agreement", g.FeeAgreementID, g.MatterID, "Fee agreement executed", true)
	return nil
}

// fileGrant returns a background publication grant for matter, asking the core for one
// and waiting for file.background_authorized when there is none.
func (s *Server) fileGrant(ctx context.Context, st Settings, matter string) (string, error) {
	get := func() string {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.fileGrants[matter]
	}
	if c := get(); c != "" {
		return c, nil
	}
	key := "file:" + matter
	s.grants.channel(key)
	if err := s.core.FileGrant(ctx, matter); err != nil {
		return "", err
	}
	// Wait no longer than one sweep interval; the sweep asks again.
	deadline := s.c.Clock.Now().Add(time.Duration(st.ReconcileMinutes) * minute)
	var cred string
	err := s.grants.wait(ctx, key, deadline, func() (bool, error) {
		cred = get()
		return cred != "", nil
	})
	return cred, err
}

// sweepPDFs retries executed signings whose signed PDF isn't stored yet.
func (s *Server) sweepPDFs(ctx context.Context, st Settings) error {
	gs, err := s.signings(ctx, `state = 'executed' AND signed_file_id IS NULL`)
	if err != nil {
		return err
	}
	for _, g := range gs {
		_ = s.storeSigned(ctx, st, g)
	}
	return nil
}
