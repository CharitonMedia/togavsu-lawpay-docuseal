package lawpaydocuseal

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/CharitonMedia/togavsu/pkg/modulekit"
)

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
	for _, want := range []string{ManifestFile, modulekit.PendingFile, "migrations/0001_lawpay.sql", "migrations/0002_docuseal.sql",
		"migrations/0003_portal.sql", "migrations/0004_fee_agreement_links.sql", "migrations/0005_payment_refund_link.sql"} {
		if _, ok := embedded[want]; !ok {
			t.Errorf("package lacks %s", want)
		}
	}
}

// Build signs the installable package with a key generated here: it is
// modulekit.BuildInstallable of the package files, it holds back exactly what
// pending.yaml lists and nothing more, the signed file passes the core's whole
// local-install check, its files pass modulekit.Check with no refusals, and a changed
// byte or another key is refused.
func TestBuildSignsWithModulekit(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signed, held, err := Build(priv)
	if err != nil {
		t.Fatalf("Build with a valid key: %v", err)
	}
	files, err := Files()
	if err != nil {
		t.Fatal(err)
	}
	again, againHeld, err := modulekit.BuildInstallable(priv, files)
	if err != nil || !bytes.Equal(signed, again) || len(againHeld) != len(held) {
		t.Fatalf("Build is not modulekit.BuildInstallable of the package files: %v", err)
	}
	entries, err := modulekit.ParsePending(files[modulekit.PendingFile])
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range modulekit.Unused(entries, held) {
		t.Errorf("pending.yaml: %s (%s) holds nothing back; the core accepts it or the package no longer has it, so drop the entry", e.Part(), e.Core)
	}
	for _, h := range held {
		t.Logf("%s", h)
	}

	var sp modulekit.SignedPackage
	if err := json.Unmarshal(signed, &sp); err != nil {
		t.Fatal(err)
	}
	pkg, err := base64.StdEncoding.DecodeString(sp.PackageBase64)
	if err != nil {
		t.Fatal(err)
	}
	if sp.ListingID != ID || sp.Version != "1.0.0" || sp.KeyID != modulekit.KeyID(pub) || sp.Digest != modulekit.Digest(pkg) {
		t.Fatalf("signed file %+v", sp)
	}
	trusted := []modulekit.TrustedKey{modulekit.Trust(pub)}
	if _, err := modulekit.Verify(pkg, sp.Digest, sp.Signature, sp.KeyID, trusted); err != nil {
		t.Fatalf("verify: %v", err)
	}
	v, ms, err := modulekit.VerifyFile(signed, trusted)
	if err != nil {
		t.Fatalf("VerifyFile: %v", err)
	}
	installable, _, err := Installable()
	if err != nil {
		t.Fatal(err)
	}
	if v.ModuleID != ID || v.Signer.KeyID != modulekit.KeyID(pub) || len(ms) != countMigrations(installable) {
		t.Fatalf("verified %+v, %d migrations", v, len(ms))
	}
	shipped, err := modulekit.ReadPackage(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := shipped.Files[modulekit.PendingFile]; ok {
		t.Fatal("the signed package ships pending.yaml")
	}
	if refs := modulekit.CheckAll(shipped.Files); len(refs) != 0 {
		t.Fatalf("modulekit.Check of the signed files: %v", refs)
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

	// modulekit.Build never signs the package with its pending list.
	if _, err := modulekit.Build(priv, files); err == nil {
		t.Fatal("modulekit.Build signed the package with its pending list")
	}
}

func countMigrations(files map[string][]byte) int {
	n := 0
	for name := range files {
		if strings.HasPrefix(name, "migrations/") {
			n++
		}
	}
	return n
}

// The module's Go source passes the core's settings source check (AC-030): no
// environment reads, and no numeric configuration literal outside fixed.go's named
// rules and the module settings.
func TestCheckSource(t *testing.T) {
	if err := modulekit.CheckSource("."); err != nil {
		t.Fatal(err)
	}
}

// The sweep falls back to reconcile_interval_minutes' own manifest default when a
// settings read fails, so that default must be a positive number of minutes.
func TestSweepFallbackIsTheSettingDefault(t *testing.T) {
	want := manifestDefaults(t)["reconcile_interval_minutes"]
	if got := settingDefault("reconcile_interval_minutes"); got != want || atoi(got) <= zero {
		t.Fatalf("settingDefault %q, manifest %q", got, want)
	}
	if got := settingDefault("no_such_setting"); got != "" {
		t.Fatalf("unknown key: %q", got)
	}
}
