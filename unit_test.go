package lawpaydocuseal

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/CharitonMedia/togavsu-lawpay-docuseal/adapters"
	"github.com/CharitonMedia/togavsu-lawpay-docuseal/fake"
	"github.com/CharitonMedia/togavsu-lawpay-docuseal/internal/fakecore"
)

func parse(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestExtractCandidates(t *testing.T) {
	charge := ExtractCandidates(parse(t, fake.ChargeWebhook(adapters.Transaction{ID: "ch_1", Status: "COMPLETED", MethodType: "card"})))
	if charge.TransactionID != "ch_1" || charge.Type != "CHARGE" || charge.Status != "COMPLETED" || charge.LooksLikeRefund() {
		t.Fatalf("charge %+v", charge)
	}
	refund := ExtractCandidates(parse(t, fake.RefundWebhook(adapters.Transaction{ID: "rf_1", Status: "COMPLETED", ChargeID: "ch_1"})))
	if refund.TransactionID != "rf_1" || refund.ChargeID != "ch_1" || !refund.LooksLikeRefund() {
		t.Fatalf("refund %+v", refund)
	}
	nested := ExtractCandidates(parse(t, []byte(`{"event":"transaction.refunded","data":{"transaction":{"id":"tx_9","type":"charge"}}}`)))
	if nested.TransactionID != "tx_9" || !nested.LooksLikeRefund() {
		t.Fatalf("nested %+v", nested)
	}
	if c := ExtractCandidates(nil); c.TransactionID != "" || c.LooksLikeRefund() {
		t.Fatalf("empty %+v", c)
	}
}

func TestDedupeKey(t *testing.T) {
	a, b := DedupeKey([]byte(`{"a":1}`)), DedupeKey([]byte(`{"a": 1}`))
	if a == b || a != DedupeKey([]byte(`{"a":1}`)) || len(a) != 64 {
		t.Fatalf("keys %s %s", a, b)
	}
}

func TestVerifyCoreWebhook(t *testing.T) {
	secret := "whsec_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	now := time.Unix(1_800_000_000, 0)
	body := []byte(`{"id":"evt_1"}`)
	skew := 5 * time.Minute
	if err := VerifyCoreWebhook(secret, fakecore.Sign(secret, now, body), "", body, now, skew); err != nil {
		t.Fatalf("t,v1 form: %v", err)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	contract := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	ts := strconv.FormatInt(now.Unix(), 10)
	if err := VerifyCoreWebhook(secret, contract, ts, body, now, skew); err != nil {
		t.Fatalf("sha256 form: %v", err)
	}
	for name, c := range map[string]struct {
		header, ts string
		body       []byte
		at         time.Time
		secret     string
	}{
		"old":          {fakecore.Sign(secret, now.Add(-6*time.Minute), body), "", body, now, secret},
		"future":       {fakecore.Sign(secret, now.Add(6*time.Minute), body), "", body, now, secret},
		"tampered":     {fakecore.Sign(secret, now, body), "", []byte(`{"id":"evt_2"}`), now, secret},
		"wrong secret": {fakecore.Sign("other", now, body), "", body, now, secret},
		"no header":    {"", "", body, now, secret},
		"no secret":    {fakecore.Sign(secret, now, body), "", body, now, ""},
		"sha256 stale": {contract, strconv.FormatInt(now.Add(-time.Hour).Unix(), 10), body, now, secret},
		"sha256 no ts": {contract, "", body, now, secret},
	} {
		if err := VerifyCoreWebhook(c.secret, c.header, c.ts, c.body, c.at, skew); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestParseDocuSeal(t *testing.T) {
	at := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	ds := fake.NewDocuSeal()
	sub, err := ds.CreateSubmission(context.Background(), adapters.SubmissionRequest{Submitters: []adapters.SubmitterRequest{{Role: adapters.RoleClient}, {Role: adapters.RoleFirm}}})
	if err != nil {
		t.Fatal(err)
	}
	form, err := ParseDocuSeal(ds.Sign(sub.ID, adapters.RoleClient, at))
	if err != nil || form.Type != "form.completed" || form.SubmissionID != sub.ID || form.Role != adapters.RoleClient || form.SubmitterID == "" {
		t.Fatalf("form %+v %v", form, err)
	}
	done, err := ParseDocuSeal(ds.Complete(sub.ID, at))
	if err != nil || done.Type != "submission.completed" || done.SubmissionID != sub.ID || done.SubmitterID != "" {
		t.Fatalf("submission %+v %v", done, err)
	}
	if form.Key() == done.Key() {
		t.Fatal("keys collide")
	}
	again, _ := ParseDocuSeal(ds.CompletedBody(sub.ID, at))
	if again.Key() != done.Key() {
		t.Fatal("same event, different key")
	}
	if _, err := ParseDocuSeal([]byte(`[1,2]`)); err == nil {
		t.Fatal("array accepted")
	}
}

func TestMoney(t *testing.T) {
	for in, want := range map[string]int64{"250": 25000, "250.5": 25050, "250.05": 25005, "$1,250.00": 125000, ".99": 99, "0": 0} {
		if got, err := ParseDollars(in); err != nil || got != want {
			t.Errorf("%q: %d %v", in, got, err)
		}
	}
	for _, in := range []string{"-5", "1.234", "1.", "abc", "1e3", "", "$-1.00"} {
		if _, err := ParseDollars(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
	for c, want := range map[int64]string{0: "$0.00", 5: "$0.05", 150000: "$1,500.00", 99999999: "$999,999.99", -20000: "-$200.00"} {
		if got := dollars(c); got != want {
			t.Errorf("dollars(%d) = %s", c, got)
		}
	}
	if plainDollars(125005) != "1250.05" || plainDollars(0) != "0.00" {
		t.Fatal("plainDollars")
	}
}
