package adapters

import (
	"encoding/json"
	"testing"
)

func TestMap(t *testing.T) {
	for _, c := range []struct {
		typ, status, method string
		pending             bool
		want                Outcome
	}{
		{"CHARGE", "COMPLETED", "card", false, OutcomeCompleted},
		{"CHARGE", "AUTHORIZED", "card", false, OutcomeCompleted},
		{"", "authorized", "bank", false, OutcomePending},
		{"CHARGE", "PENDING", "bank", false, OutcomePending},
		{"CHARGE", "COMPLETED", "bank", false, OutcomeCompleted},
		{"CHARGE", "AUTHORIZED", "bank", true, OutcomeNone},
		{"CHARGE", "SETTLED", "bank", true, OutcomeCompleted},
		{"CHARGE", "DECLINED", "card", false, OutcomeDeclined},
		{"CHARGE", "FAILED", "bank", true, OutcomeFailed},
		{"REFUND", "COMPLETED", "card", false, OutcomeRefundDone},
		{"REFUND", "FAILED", "card", false, OutcomeNone},
		{"CHARGE", "SOMETHING_NEW", "card", false, OutcomeNone},
	} {
		if got := Map(Transaction{Type: c.typ, Status: c.status, MethodType: c.method}, c.pending); got != c.want {
			t.Errorf("%s %s %s pending=%v: %s, want %s", c.typ, c.status, c.method, c.pending, got, c.want)
		}
	}
}

func TestMethod(t *testing.T) {
	if Method("bank") != "ach" || Method("ACH") != "ach" || Method("card") != "card" || Method("") != "card" {
		t.Fatal("method map")
	}
}

func TestParseCents(t *testing.T) {
	for in, want := range map[string]int64{"0": 0, "250": 250, " 99999999 ": 99999999} {
		if got, err := ParseCents(in); err != nil || got != want {
			t.Errorf("%q: %d %v", in, got, err)
		}
	}
	for _, in := range []string{"", "-5", "+5", "2.50", "1e3", "12a", "99999999999999999999"} {
		if _, err := ParseCents(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
	for raw, want := range map[string]int64{`1500`: 1500, `"1500"`: 1500, `null`: 0, ``: 0} {
		if got, err := ParseCentsJSON(json.RawMessage(raw)); err != nil || got != want {
			t.Errorf("JSON %s: %d %v", raw, got, err)
		}
	}
	for _, raw := range []string{`15.5`, `-1`, `"abc"`} {
		if _, err := ParseCentsJSON(json.RawMessage(raw)); err == nil {
			t.Errorf("JSON %s accepted", raw)
		}
	}
}
