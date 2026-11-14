// Package coreitems names the core changes (docs/modules/CORE-CHANGES.md in the core
// repository) this module needs and which of them the pinned core (go.mod) does not build
// yet, so the tests that run on the real core can skip a criterion until its items land.
// It is for skipping tests only: what the package holds back is package/pending.yaml's
// business (modulekit.Installable). The kit can't report which CORE items a core builds,
// so Unbuilt is kept by hand.
package coreitems

import (
	"fmt"
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

// Install is every core item a part of the package waits on: each core item
// package/pending.yaml names. Until they land the pinned core installs the package with
// those parts held back (modulekit.Installable), so the module runs without its provider
// webhooks, processor registration, notifications, client tabs, mail, books and client
// access scopes and its fee agreement and payment refund links; the criteria need the
// whole module, so every one of them counts these items too. The package tests check this
// list against pending.yaml.
var Install = []string{"CORE-02", "CORE-03", "CORE-04", "CORE-07", "CORE-08", "CORE-10", "CORE-14", "CORE-22", "CORE-23", "CORE-24"}

// Skip is why a test that needs the given items can't run on the pinned core yet, or ""
// when it can. Every test also needs the install, so the install items count too.
func Skip(need ...string) string {
	own := Missing(need...)
	install := Missing(Install...)
	switch {
	case len(own) > 0 && len(install) > 0:
		return fmt.Sprintf("needs %s; the install needs %s", strings.Join(own, ", "), strings.Join(install, ", "))
	case len(own) > 0:
		return "needs " + strings.Join(own, ", ")
	case len(install) > 0:
		return fmt.Sprintf("needs %s for the install", strings.Join(install, ", "))
	}
	return ""
}

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
