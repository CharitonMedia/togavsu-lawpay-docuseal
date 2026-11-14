package coreitems

import (
	"strings"
	"testing"

	"github.com/CharitonMedia/togavsu/pkg/modulekit"

	lawpaydocuseal "github.com/CharitonMedia/togavsu-lawpay-docuseal"
)

// Install is exactly the core items package/pending.yaml names, and every part the
// pinned core holds back waits on an item listed as unbuilt, so no criterion runs on a
// core that still holds back part of the module.
func TestInstallIsThePendingList(t *testing.T) {
	files, err := lawpaydocuseal.Files()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := modulekit.ParsePending(files[modulekit.PendingFile])
	if err != nil {
		t.Fatal(err)
	}
	_, held, err := lawpaydocuseal.Installable()
	if err != nil {
		t.Fatal(err)
	}
	install, pending := map[string]bool{}, map[string]bool{}
	for _, item := range Install {
		install[item] = true
	}
	for _, e := range entries {
		pending[e.Core] = true
		if !install[e.Core] {
			t.Errorf("pending.yaml %s waits on %s, which is not in Install", e.Part(), e.Core)
		}
	}
	for _, item := range Install {
		if !pending[item] {
			t.Errorf("Install has %s, but no pending.yaml entry waits on it", item)
		}
	}
	for _, h := range held {
		if _, ok := Unbuilt[h.Pending.Core]; !ok {
			t.Errorf("%s is held back for %s, which Unbuilt counts as built", h.Part, h.Pending.Core)
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
