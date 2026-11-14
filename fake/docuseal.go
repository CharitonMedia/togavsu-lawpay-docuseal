package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/CharitonMedia/togavsu-lawpay-docuseal/adapters"
)

// DocuSeal is an in-memory DocuSeal. The zero value is not usable; call NewDocuSeal.
type DocuSeal struct {
	mu   sync.Mutex
	seq  int
	subs map[string]*adapters.Submission
	next []string // scripted outcomes: create_fail, archive_fail, pdf_fail, read_fail
	// Gate, when set, is waited on inside CreateSubmission (tests use it to hold a send).
	Gate chan struct{}
	// OnArchive, when set, runs at the start of Archive, outside the fake's lock (tests use
	// it to act while a signing is voiding).
	OnArchive func(id string)

	CreateCalls, ArchiveCalls, ReadCalls, PDFCalls int
	Requests                                       []adapters.SubmissionRequest
	Archived                                       []string
}

// NewDocuSeal returns an empty fake.
func NewDocuSeal() *DocuSeal { return &DocuSeal{subs: map[string]*adapters.Submission{}} }

// Script queues outcomes for the next calls, in order.
func (f *DocuSeal) Script(outcomes ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next = append(f.next, outcomes...)
}

func (f *DocuSeal) take(kind string) bool {
	if len(f.next) > 0 && f.next[0] == kind {
		f.next = f.next[1:]
		return true
	}
	return false
}

func (f *DocuSeal) id() string {
	f.seq++
	return fmt.Sprintf("%d", 1000+f.seq)
}

func notFound() error {
	return &adapters.DocuSealError{HTTPStatus: http.StatusNotFound, Message: "Not found"}
}

// CreateSubmission stores a submission with its submitters, pending.
func (f *DocuSeal) CreateSubmission(_ context.Context, r adapters.SubmissionRequest) (adapters.Submission, error) {
	if f.Gate != nil {
		<-f.Gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.CreateCalls++
	f.Requests = append(f.Requests, r)
	if f.take("create_fail") {
		return adapters.Submission{}, &adapters.DocuSealError{HTTPStatus: http.StatusUnprocessableEntity, Message: "Template not found"}
	}
	s := &adapters.Submission{ID: f.id(), Status: "pending", ExpireAt: r.ExpireAt}
	for _, sub := range r.Submitters {
		sid := f.id()
		s.Submitters = append(s.Submitters, adapters.Submitter{ID: sid, Role: sub.Role, Status: "awaiting",
			SigningURL: "https://docuseal.invalid/s/" + sid})
	}
	f.subs[s.ID] = s
	return *s, nil
}

// Submission reads one submission.
func (f *DocuSeal) Submission(_ context.Context, id string) (adapters.Submission, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ReadCalls++
	if f.take("read_fail") {
		return adapters.Submission{}, &adapters.DocuSealError{HTTPStatus: http.StatusBadGateway, Message: "Bad gateway"}
	}
	s, ok := f.subs[id]
	if !ok {
		return adapters.Submission{}, notFound()
	}
	return *s, nil
}

// SyntheticPDF is the bytes the fake serves for a document.
func SyntheticPDF(kind, id string) []byte {
	return []byte("%PDF-1.4\n% synthetic " + kind + " fee agreement for submission " + id + "\n%%EOF\n")
}

// CurrentPDF returns the agreement as sent.
func (f *DocuSeal) CurrentPDF(_ context.Context, id string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.PDFCalls++
	if f.take("pdf_fail") {
		return nil, &adapters.DocuSealError{HTTPStatus: http.StatusBadGateway, Message: "Bad gateway"}
	}
	if _, ok := f.subs[id]; !ok {
		return nil, notFound()
	}
	return SyntheticPDF("unsigned", id), nil
}

// CompletedPDF returns the executed agreement.
func (f *DocuSeal) CompletedPDF(_ context.Context, id string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.PDFCalls++
	if f.take("pdf_fail") {
		return nil, &adapters.DocuSealError{HTTPStatus: http.StatusBadGateway, Message: "Bad gateway"}
	}
	if _, ok := f.subs[id]; !ok {
		return nil, notFound()
	}
	return SyntheticPDF("executed", id), nil
}

// Archive archives a submission.
func (f *DocuSeal) Archive(_ context.Context, id string) error {
	if f.OnArchive != nil {
		f.OnArchive(id)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ArchiveCalls++
	if f.take("archive_fail") {
		return &adapters.DocuSealError{HTTPStatus: http.StatusBadGateway, Message: "Bad gateway"}
	}
	s, ok := f.subs[id]
	if !ok {
		return notFound()
	}
	s.Status = "archived"
	f.Archived = append(f.Archived, id)
	return nil
}

// Template reads a template; every id exists.
func (f *DocuSeal) Template(_ context.Context, _ int64) error { return nil }

// Calls returns the number of adapter calls of every kind so far.
func (f *DocuSeal) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.CreateCalls + f.ArchiveCalls + f.ReadCalls + f.PDFCalls
}

// Last returns the newest submission.
func (f *DocuSeal) Last() adapters.Submission {
	f.mu.Lock()
	defer f.mu.Unlock()
	var best *adapters.Submission
	for _, s := range f.subs {
		if best == nil || len(s.ID) > len(best.ID) || (len(s.ID) == len(best.ID) && s.ID > best.ID) {
			best = s
		}
	}
	if best == nil {
		return adapters.Submission{}
	}
	return *best
}

func (f *DocuSeal) mark(id, role, status string) adapters.Submitter {
	s := f.subs[id]
	for i := range s.Submitters {
		if strings.EqualFold(s.Submitters[i].Role, role) {
			s.Submitters[i].Status = status
			return s.Submitters[i]
		}
	}
	return adapters.Submitter{}
}

func formBody(event string, sub adapters.Submitter, submission string, at time.Time) []byte {
	b, _ := json.Marshal(map[string]any{"event_type": event, "timestamp": at.UTC().Format(time.RFC3339Nano),
		"data": map[string]any{"id": json.Number(sub.ID), "submission_id": json.Number(submission), "role": sub.Role,
			"status": sub.Status, "submission": map[string]any{"id": json.Number(submission)}}})
	return b
}

func (f *DocuSeal) submissionBody(event, id string, at time.Time) []byte {
	s := f.subs[id]
	var subs []map[string]any
	for _, sub := range s.Submitters {
		subs = append(subs, map[string]any{"id": json.Number(sub.ID), "role": sub.Role, "status": sub.Status})
	}
	b, _ := json.Marshal(map[string]any{"event_type": event, "timestamp": at.UTC().Format(time.RFC3339Nano),
		"data": map[string]any{"id": json.Number(id), "status": s.Status, "submitters": subs}})
	return b
}

// Sign marks role's submitter completed and returns the form.completed webhook body.
func (f *DocuSeal) Sign(id, role string, at time.Time) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	sub := f.mark(id, role, "completed")
	return formBody("form.completed", sub, id, at)
}

// Decline marks the client declined and returns the form.declined webhook body.
func (f *DocuSeal) Decline(id string, at time.Time) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	sub := f.mark(id, adapters.RoleClient, "declined")
	f.subs[id].Status = "declined"
	return formBody("form.declined", sub, id, at)
}

// Complete marks every submitter completed and returns the submission.completed body.
func (f *DocuSeal) Complete(id string, at time.Time) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.subs[id]
	for i := range s.Submitters {
		s.Submitters[i].Status = "completed"
	}
	s.Status = "completed"
	return f.submissionBody("submission.completed", id, at)
}

// CompletedBody returns a submission.completed body without changing the submission (a
// premature or forged completion).
func (f *DocuSeal) CompletedBody(id string, at time.Time) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.submissionBody("submission.completed", id, at)
}

// Expire marks the submission expired and returns the submission.expired body.
func (f *DocuSeal) Expire(id string, at time.Time) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs[id].Status = "expired"
	return f.submissionBody("submission.expired", id, at)
}

// OrphanBody is a body for a submission DocuSeal has but the module never made.
func OrphanBody(event string, at time.Time) []byte {
	b, _ := json.Marshal(map[string]any{"event_type": event, "timestamp": at.UTC().Format(time.RFC3339Nano),
		"data": map[string]any{"id": json.Number("999999"), "status": "completed"}})
	return b
}
