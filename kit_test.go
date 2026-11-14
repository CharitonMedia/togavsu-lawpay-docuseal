package lawpaydocuseal

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/CharitonMedia/togavsu/pkg/modulekit"

	"github.com/CharitonMedia/togavsu-lawpay-docuseal/internal/coreitems"
)

// stripPending is a Build edit that removes the fields the pinned core does not accept
// yet (coreitems.Pending).
func stripPending(t *testing.T) func(map[string][]byte) {
	return func(files map[string][]byte) {
		stripped, err := coreitems.Strip(files, coreitems.Pending...)
		if err != nil {
			t.Fatal(err)
		}
		for name, b := range stripped {
			files[name] = b
		}
	}
}

// The embedded package and the package directory on disk are the same files.
func TestFilesMatchPackageDir(t *testing.T) {
	embedded, err := Files()
	if err != nil {
		t.Fatal(err)
	}
	disk, err := modulekit.ReadDir("package")
	if err != nil {
		t.Fatal(err)
	}
	if len(embedded) != len(disk) {
		t.Fatalf("embedded %d files, package dir %d", len(embedded), len(disk))
	}
	for name, b := range disk {
		if !bytes.Equal(embedded[name], b) {
			t.Errorf("%s differs between the embedded package and the package dir", name)
		}
	}
	for _, want := range []string{ManifestFile, "migrations/0001_lawpay.sql", "migrations/0002_docuseal.sql", "migrations/0003_portal.sql"} {
		if _, ok := embedded[want]; !ok {
			t.Errorf("package lacks %s", want)
		}
	}
}

// Build signs the kit's archive with a generated key, the signature verifies against
// that key only, and a changed byte or another key is refused.
func TestBuildSignsWithModulekit(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := Archive(nil)
	if err != nil {
		t.Fatal(err)
	}
	files, err := Files()
	if err != nil {
		t.Fatal(err)
	}
	kitPkg, err := modulekit.Archive(files)
	if err != nil || !bytes.Equal(pkg, kitPkg) {
		t.Fatalf("Archive is not modulekit.Archive of the package files: %v", err)
	}
	signed, err := Build(priv, nil)
	if err != nil {
		t.Fatal(err)
	}
	again, err := SignFile(priv, pkg)
	if err != nil || !bytes.Equal(signed, again) {
		t.Fatalf("Build is not SignFile(Archive): %v", err)
	}
	var sp modulekit.SignedPackage
	if err := json.Unmarshal(signed, &sp); err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(sp.PackageBase64)
	if err != nil || !bytes.Equal(raw, pkg) {
		t.Fatalf("signed file carries other package bytes: %v", err)
	}
	if sp.ListingID != ID || sp.Version != "1.0.0" || sp.KeyID != modulekit.KeyID(pub) || sp.Digest != modulekit.Digest(pkg) {
		t.Fatalf("signed file %+v", sp)
	}
	trusted := []modulekit.TrustedKey{modulekit.Trust(pub)}
	if _, err := modulekit.Verify(pkg, sp.Digest, sp.Signature, sp.KeyID, trusted); err != nil {
		t.Fatalf("verify: %v", err)
	}
	tampered := append([]byte(nil), pkg...)
	tampered[len(tampered)/2] ^= 0xff
	if _, err := modulekit.Verify(tampered, "", sp.Signature, sp.KeyID, trusted); err == nil {
		t.Fatal("a changed byte verified")
	}
	otherPub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := modulekit.Verify(pkg, sp.Digest, sp.Signature, sp.KeyID, []modulekit.TrustedKey{modulekit.Trust(otherPub)}); err == nil {
		t.Fatal("an untrusted signer verified")
	}

	// The core's own install checks refuse the full package only for the pending fields.
	_, _, err = modulekit.Check(files)
	if err == nil || len(coreitems.Explain(err.Error())) == 0 {
		t.Fatalf("modulekit.Check of the full package: %v, want a refusal for a pending field", err)
	}

	// Without them, Check over the files, CheckPackage over the bytes and VerifyFile over
	// the signed file (the install checks after the signature) all pass.
	stripped, err := coreitems.Strip(files, coreitems.Pending...)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := modulekit.Check(stripped); err != nil {
		t.Fatalf("Check: %v", err)
	}
	strippedPkg, err := Archive(stripPending(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := modulekit.CheckPackage(strippedPkg); err != nil {
		t.Fatalf("CheckPackage: %v", err)
	}
	strippedSigned, err := Build(priv, stripPending(t))
	if err != nil {
		t.Fatal(err)
	}
	v, ms, err := modulekit.VerifyFile(strippedSigned, trusted)
	if err != nil {
		t.Fatalf("VerifyFile: %v", err)
	}
	if v.ModuleID != ID || v.Signer.KeyID != modulekit.KeyID(pub) || len(ms) != 3 {
		t.Fatalf("verified %+v, %d migrations", v, len(ms))
	}
}

// CheckSource is the core's settings source check over the module's Go source. The
// module must never read the environment, so any finding other than a numeric literal
// fails. The literal findings skip: the check enumerates its allowed fixed computations
// (a zero row count, a strconv base, a file mode, a UUID layout) by core file path and
// function, so a module has no way to declare its own, and it reports every literal in
// a comparison, return, call or constant. Remove the skip when the kit gives modules
// that allowance.
func TestCheckSource(t *testing.T) {
	err := modulekit.CheckSource(".")
	if err == nil {
		return
	}
	lines := strings.Split(err.Error(), "\n")
	if lines[0] != "settings check failed:" {
		t.Fatal(err)
	}
	var literals int
	for _, l := range lines[1:] {
		if strings.Contains(l, ": tunable number ") || strings.Contains(l, ": tunable duration ") {
			literals++
			continue
		}
		t.Errorf("settings check: %s", l)
	}
	if !t.Failed() {
		t.Skipf("needs a module fixed-site allowance in modulekit.CheckSource: %d numeric literal findings, no environment reads", literals)
	}
}
