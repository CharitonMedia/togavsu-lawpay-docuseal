package coreitems

import (
	"fmt"
	"strings"
)

const manifestFile = "togavsu-module.yaml"

// Field is a part of the package, from the module spec, that the pinned core refuses
// until its core item lands. In the manifest, Cut lists trimmed lines that carry the
// field; each goes with the lines indented under it. In a migration, Cut lists text that
// is removed where it appears.
type Field struct {
	Item string
	Name string
	File string
	Cut  []string
}

// Pending is every such field in the package directory, in one place. The package tests
// strip them to run the core's check on the rest, and check that each one alone is still
// refused for its item, so a field the core starts to accept fails the tests until it
// leaves this list.
//
// The scopes books:read (CORE-02), mail:send (CORE-04) and client_access:read (CORE-14)
// are not here: the package check accepts any scope name, and the core refuses an
// unknown one only when the install issues the module token.
var Pending = []Field{
	{"CORE-23", "payment_processor", manifestFile, []string{"payment_processor:"}},
	{"CORE-07", "inbound_webhooks", manifestFile, []string{"inbound_webhooks:"}},
	{"CORE-03", "notifications", manifestFile, []string{"notifications:"}},
	{"CORE-08", "sidebar section, icon and order", manifestFile, []string{"section: money", "icon: payment", "order: 400"}},
	{"CORE-10", "widget target params", manifestFile, []string{"params: [state]", "params: [kind]"}},
	{"CORE-10", "tab params", manifestFile, []string{"params: [state, kind]"}},
	{"CORE-10", "client tabs pay and sign (placement client, nav)", manifestFile, []string{"- id: pay", "- id: sign"}},
	{"CORE-22", "data extends on record fee_agreement", manifestFile, []string{"extends:"}},
	{"CORE-24", "refunds.core_refund_id references core.payment_refunds", "migrations/0001_lawpay.sql",
		[]string{" REFERENCES core.payment_refunds (id) ON DELETE RESTRICT"}},
	{"CORE-22", "signing tables reference core.fee_agreements", "migrations/0002_docuseal.sql",
		[]string{" REFERENCES core.fee_agreements (id) ON DELETE RESTRICT"}},
}

// Strip returns a copy of the package files without the given fields. It fails when a
// field is not in its file, so an entry cannot go stale unnoticed.
func Strip(files map[string][]byte, fields ...Field) (map[string][]byte, error) {
	out := make(map[string][]byte, len(files))
	for name, b := range files {
		out[name] = b
	}
	for _, f := range fields {
		src, ok := out[f.File]
		if !ok {
			return nil, fmt.Errorf("%s: no file %s", f.Name, f.File)
		}
		var got string
		var found bool
		if f.File == manifestFile {
			got, found = cutLines(string(src), f.Cut)
		} else {
			got = string(src)
			for _, c := range f.Cut {
				found = found || strings.Contains(got, c)
				got = strings.ReplaceAll(got, c, "")
			}
		}
		if !found {
			return nil, fmt.Errorf("%s: not in %s", f.Name, f.File)
		}
		out[f.File] = []byte(got)
	}
	return out, nil
}

// cutLines drops each line whose trimmed text is in cut, with the lines indented under it.
func cutLines(src string, cut []string) (string, bool) {
	drop := map[string]bool{}
	for _, c := range cut {
		drop[c] = true
	}
	var out []string
	found, inside := false, false
	depth := zero
	for _, l := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(l)
		indent := len(l) - len(strings.TrimLeft(l, " "))
		if inside && (trimmed == "" || indent > depth) {
			continue
		}
		inside = false
		if drop[trimmed] {
			found, inside, depth = true, true, indent
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n"), found
}
