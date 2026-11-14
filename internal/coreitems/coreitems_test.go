package coreitems

import (
	"strings"
	"testing"
)

// Every pending package field is an install prerequisite: the core refuses the package
// until its item lands.
func TestPendingItemsAreInstallItems(t *testing.T) {
	install := map[string]bool{}
	for _, item := range Install {
		install[item] = true
	}
	for _, f := range Pending {
		if !install[f.Item] {
			t.Errorf("%s (%s) is pending in the package but not in Install", f.Name, f.Item)
		}
	}
}

// A test that names no item still skips while CORE-24 is unbuilt, even once every
// other install item has landed, because the migrations link to core.payment_refunds.
func TestSkipNeedsCore24ForTheInstall(t *testing.T) {
	saved := Unbuilt
	t.Cleanup(func() { Unbuilt = saved })
	Unbuilt = map[string]string{"CORE-24": saved["CORE-24"]}
	if why := Skip(); why != "needs CORE-24 for the install" {
		t.Fatalf("Skip() = %q", why)
	}
	if why := Skip("CORE-23"); why != "needs CORE-24 for the install" {
		t.Fatalf("Skip(CORE-23) = %q", why)
	}
	Unbuilt = map[string]string{}
	if why := Skip("CORE-23"); why != "" {
		t.Fatalf("with every item built: %q", why)
	}
	Unbuilt = map[string]string{"CORE-01": "x", "CORE-24": "y"}
	if why := Skip("CORE-01"); !strings.HasPrefix(why, "needs CORE-01; the install needs CORE-24") {
		t.Fatalf("Skip(CORE-01) = %q", why)
	}
}
