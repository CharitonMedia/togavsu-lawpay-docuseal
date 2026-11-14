package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CharitonMedia/togavsu/pkg/modulekit"

	lawpaydocuseal "github.com/CharitonMedia/togavsu-lawpay-docuseal"
	"github.com/CharitonMedia/togavsu-lawpay-docuseal/internal/coreitems"
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

// pendingDir writes the package files without the given pending fields.
func pendingDir(t *testing.T, fields ...coreitems.Field) string {
	t.Helper()
	files, err := lawpaydocuseal.Files()
	if err != nil {
		t.Fatal(err)
	}
	stripped, err := coreitems.Strip(files, fields...)
	if err != nil {
		t.Fatal(err)
	}
	return writeDir(t, stripped)
}

// The package subcommand refuses the full package only for the fields the pinned core
// does not accept yet, and signs the rest with the key file's seed into a file that
// passes the core's whole local-install check.
func TestPackageBuildsAndVerifies(t *testing.T) {
	key, pub := keyFile(t)
	signed, _, err := buildSigned(key, "")
	if err == nil || signed != nil || len(coreitems.Explain(err.Error())) == 0 {
		t.Fatalf("the full package: %v, want a refusal for a pending field", err)
	}
	signed, got, err := buildSigned(key, pendingDir(t, coreitems.Pending...))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(pub) {
		t.Fatal("signed with another key")
	}
	if _, _, err := modulekit.VerifyFile(signed, []modulekit.TrustedKey{modulekit.Trust(pub)}); err != nil {
		t.Fatalf("VerifyFile: %v", err)
	}
}

// Each pending field alone is still refused, for its own item. When the core starts to
// accept one, this fails until the field leaves coreitems.Pending.
func TestPendingFieldsStillRefused(t *testing.T) {
	for i, f := range coreitems.Pending {
		var rest []coreitems.Field
		rest = append(rest, coreitems.Pending[:i]...)
		rest = append(rest, coreitems.Pending[i+1:]...)
		files, err := modulekit.ReadDir(pendingDir(t, rest...))
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = modulekit.Check(files)
		if err == nil {
			t.Errorf("%s (%s): the core accepts it now; drop it from coreitems.Pending", f.Name, f.Item)
			continue
		}
		if items := coreitems.Explain(err.Error()); !contains(items, f.Item) {
			t.Errorf("%s (%s): refused for %v: %v", f.Name, f.Item, items, err)
		}
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// It refuses a bad key file and a package the core's check refuses, and signs nothing.
func TestPackageRefuses(t *testing.T) {
	key, _ := keyFile(t)
	dir := pendingDir(t, coreitems.Pending...)
	bad := filepath.Join(t.TempDir(), "bad.key")
	if err := os.WriteFile(bad, []byte("not a seed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := buildSigned(bad, dir); err == nil {
		t.Fatal("signed with a bad key file")
	}
	if _, _, err := buildSigned(filepath.Join(t.TempDir(), "missing.key"), dir); err == nil {
		t.Fatal("signed with no key file")
	}
	files, err := modulekit.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files[lawpaydocuseal.ManifestFile] = []byte(strings.Replace(string(files[lawpaydocuseal.ManifestFile]), "id: lawpay-docuseal\n", "id: Not A Slug\n", 1))
	signed, _, err := buildSigned(key, writeDir(t, files))
	if err == nil || signed != nil || !strings.Contains(err.Error(), "package refused (validation)") {
		t.Fatalf("a refused package: %v", err)
	}
}
