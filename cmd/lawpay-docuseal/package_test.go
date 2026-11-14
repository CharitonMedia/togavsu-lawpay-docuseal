package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CharitonMedia/togavsu/pkg/modulekit"

	lawpaydocuseal "github.com/CharitonMedia/togavsu-lawpay-docuseal"
)

// keyFile writes a generated seed as a key file and returns its path and public key.
func keyFile(t *testing.T) (string, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "signing.key")
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(priv.Seed())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, pub
}

// writeDir writes package files under a new directory.
func writeDir(t *testing.T, files map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	for name, b := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// packageFiles is the module's package files, pending.yaml included.
func packageFiles(t *testing.T) map[string][]byte {
	t.Helper()
	files, err := lawpaydocuseal.Files()
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// The package subcommand always builds and signs, with a key generated here, a package
// the pinned core accepts: it holds back each part pending.yaml lists that the core
// refuses, the signed file passes the core's whole local-install check, and its files
// pass modulekit.Check with no refusals. It fails when pending.yaml misses a refusal (the
// build then errors) or holds an entry that holds nothing back.
func TestPackageBuildsAndVerifies(t *testing.T) {
	key, pub := keyFile(t)
	for _, dir := range []string{"", writeDir(t, packageFiles(t))} {
		b, err := buildSigned(key, dir)
		if err != nil {
			t.Fatalf("build with a valid key (dir %q): %v", dir, err)
		}
		if !b.pub.Equal(pub) {
			t.Fatal("signed with another key")
		}
		for _, e := range b.unused {
			t.Errorf("pending.yaml: %s (%s) holds nothing back; drop the entry", e.Part(), e.Core)
		}
		if len(b.held) == 0 {
			t.Error("nothing held back, but the pinned core refuses parts of the package; is pending.yaml read?")
		}
		v, _, err := modulekit.VerifyFile(b.signed, []modulekit.TrustedKey{modulekit.Trust(pub)})
		if err != nil {
			t.Fatalf("VerifyFile: %v", err)
		}
		if v.ModuleID != lawpaydocuseal.ID {
			t.Fatalf("verified %+v", v)
		}
		var sp modulekit.SignedPackage
		if err := json.Unmarshal(b.signed, &sp); err != nil {
			t.Fatal(err)
		}
		raw, err := base64.StdEncoding.DecodeString(sp.PackageBase64)
		if err != nil {
			t.Fatal(err)
		}
		pkg, err := modulekit.ReadPackage(raw)
		if err != nil {
			t.Fatal(err)
		}
		if refs := modulekit.CheckAll(pkg.Files); len(refs) != 0 {
			t.Fatalf("modulekit.Check of the signed files: %v", refs)
		}
	}
}

// A pending list that misses a refusal fails the build and names the refusal; an entry
// for a part the core accepts is reported as holding nothing back.
func TestPackagePendingListIsChecked(t *testing.T) {
	key, _ := keyFile(t)
	files := packageFiles(t)
	src := string(files[modulekit.PendingFile])
	entry := "  - field: notifications\n    core: CORE-03\n    reason: Declares the exception and proposed refund notification kinds.\n"
	if !strings.Contains(src, entry) {
		t.Fatal("pending.yaml has no notifications entry to drop")
	}
	files[modulekit.PendingFile] = []byte(strings.Replace(src, entry, "", 1))
	_, err := buildSigned(key, writeDir(t, files))
	if err == nil || !strings.Contains(err.Error(), "togavsu-module.yaml notifications: unknown_field") {
		t.Fatalf("a pending list without notifications: %v", err)
	}

	files = packageFiles(t)
	files[modulekit.PendingFile] = append(files[modulekit.PendingFile],
		[]byte("  - field: help.heading\n    core: CORE-99\n    reason: Accepted already.\n")...)
	b, err := buildSigned(key, writeDir(t, files))
	if err != nil {
		t.Fatal(err)
	}
	if len(b.unused) != 1 || b.unused[0].Field != "help.heading" {
		t.Fatalf("unused %+v", b.unused)
	}
}

// It refuses a bad key file and a package the core's check refuses, and signs nothing.
func TestPackageRefuses(t *testing.T) {
	key, _ := keyFile(t)
	dir := writeDir(t, packageFiles(t))
	bad := filepath.Join(t.TempDir(), "bad.key")
	if err := os.WriteFile(bad, []byte("not a seed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := buildSigned(bad, dir); err == nil {
		t.Fatal("signed with a bad key file")
	}
	if _, err := buildSigned(filepath.Join(t.TempDir(), "missing.key"), dir); err == nil {
		t.Fatal("signed with no key file")
	}
	files := packageFiles(t)
	files[lawpaydocuseal.ManifestFile] = []byte(strings.Replace(string(files[lawpaydocuseal.ManifestFile]), "id: lawpay-docuseal\n", "id: Not A Slug\n", 1))
	b, err := buildSigned(key, writeDir(t, files))
	if err == nil || b.signed != nil || !strings.Contains(err.Error(), "package refused (validation)") {
		t.Fatalf("a refused package: %v", err)
	}
}
