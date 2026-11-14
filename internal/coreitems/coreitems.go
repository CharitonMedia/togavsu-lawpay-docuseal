// Package coreitems names the core changes (docs/modules/CORE-CHANGES.md in the core
// repository) this module needs and which of them the pinned core (go.mod) does not build
// yet, so the tests that run on the real core can skip a criterion until its items land.
package coreitems

import (
	"sort"
	"strings"
)

// Unbuilt is every core change this module needs that the pinned core does not build,
// with what it adds. Remove an entry when the go.mod pin moves to a core that builds it;
// each criterion whose items are then all built runs on the next test run.
var Unbuilt = map[string]string{
	"CORE-01": "service calls with no viewer",
	"CORE-02": "books:read and the books reader",
	"CORE-03": "module notification kinds and createNotification audiences",
	"CORE-04": "module mail (POST /api/v1/mail/messages, mail:send)",
	"CORE-05": "system mail routing",
	"CORE-07": "inbound provider webhooks (manifest inbound_webhooks)",
	"CORE-08": "sidebar section, icon and order",
	"CORE-09": "the client frame sandbox",
	"CORE-10": "client tabs (placement client, params, nav)",
	"CORE-12": "client trust reads and the replenishment link",
	"CORE-14": "client access read (client_access:read)",
	"CORE-22": "fee_agreement and payment_refund as linkable core records",
	"CORE-23": "payment processors (manifest payment_processor, processor grants)",
	"CORE-24": "payment refunds",
	"CORE-25": "the fee agreement signature callback",
	// No item in CORE-CHANGES.md emits this event; the core's conflict decision is an
	// attorney-only audit change with action conflict.decided.
	"conflict_check.decided": "a core conflict_check.decided event with the check's decision",
}

// Install is what the package itself needs before the core installs and deploys it: the
// manifest fields and linkable records the install check reads, and the scopes the
// module token is issued with.
var Install = []string{"CORE-02", "CORE-03", "CORE-04", "CORE-07", "CORE-08", "CORE-10", "CORE-14", "CORE-22", "CORE-23"}

// Missing returns the items in need that are not built, sorted, without duplicates.
func Missing(need ...string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range need {
		if _, ok := Unbuilt[n]; ok && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// gaps maps a refusal the pinned core gives this package to the items that remove it.
var gaps = []struct {
	text  string
	items []string
}{
	{`unknown field "inbound_webhooks"`, []string{"CORE-07"}},
	{`unknown field "payment_processor"`, []string{"CORE-23"}},
	{`unknown field "notifications"`, []string{"CORE-03"}},
	{`unknown field "section"`, []string{"CORE-08"}},
	{`unknown field "icon"`, []string{"CORE-08"}},
	{`unknown field "order"`, []string{"CORE-08"}},
	{`unknown field "params"`, []string{"CORE-10"}},
	{`unknown field "nav"`, []string{"CORE-10"}},
	{"placement is home or matter", []string{"CORE-10"}},
	{"unknown core record fee_agreement", []string{"CORE-22"}},
	{"unknown core record payment_refund", []string{"CORE-22"}},
	{"core.fee_agreements", []string{"CORE-22"}},
	{"core.payment_refunds", []string{"CORE-22", "CORE-24"}},
	// books:read, mail:send and client_access:read are not in the core's scope catalog.
	{"unknown scope", []string{"CORE-02", "CORE-04", "CORE-14"}},
}

// Explain returns the unbuilt items a core refusal points to, or nil when no missing
// item explains it (a real defect in the package).
func Explain(refusal string) []string {
	for _, g := range gaps {
		if strings.Contains(refusal, g.text) {
			return Missing(g.items...)
		}
	}
	return nil
}
