// Package lawpaydocuseal is the LawPay and DocuSeal module for Togavsu: LawPay card and ACH
// payments into the firm's operating and trust accounts, full and partial refunds,
// reconciliation of LawPay against the core, and DocuSeal e-signature for fee agreements.
//
// The module runs in its own container. It reaches core records only through the core API,
// with its own module token plus the viewer's context token on interactive calls, a
// core-issued task authorization on background writes, and service calls (the books reader)
// on background reads. Money never moves outside the core: a LawPay charge becomes a core
// payment and a LawPay refund becomes a core payment refund. Its own records live in schema
// mod_lawpay_docuseal, linked to core ids by ON DELETE RESTRICT foreign keys.
//
// Packaging and signing go through the core's public module kit (pkg/modulekit): Archive
// writes the installable tar and Build signs it, so the package is checked and signed by
// the code the core runs at install.
package lawpaydocuseal

import (
	"crypto/ed25519"
	"embed"
	"io/fs"

	"github.com/CharitonMedia/togavsu/pkg/modulekit"
)

// ID is the module id from the manifest.
const ID = "lawpay-docuseal"

// ManifestFile is the manifest's name inside the package.
const ManifestFile = "togavsu-module.yaml"

//go:embed package
var packageFS embed.FS

// Files returns the package files (manifest and migrations) by package path, read with
// the kit's rules (names starting with "." or "_" left out), so the embedded package and
// the package directory on disk ship the same bytes.
func Files() (map[string][]byte, error) {
	dir, err := fs.Sub(packageFS, "package")
	if err != nil {
		return nil, err
	}
	return modulekit.Files(dir)
}

// Archive writes the installable package, the gzip tar the core installs
// (modulekit.Archive). edit, when set, changes the files first; tests use it for
// tampered and refused variants.
func Archive(edit func(map[string][]byte)) ([]byte, error) {
	files, err := Files()
	if err != nil {
		return nil, err
	}
	if edit != nil {
		edit(files)
	}
	return modulekit.Archive(files)
}

// Build writes the package signed with priv as the signed package file the admin
// installs: Archive, then SignFile.
func Build(priv ed25519.PrivateKey, edit func(map[string][]byte)) ([]byte, error) {
	pkg, err := Archive(edit)
	if err != nil {
		return nil, err
	}
	return SignFile(priv, pkg)
}

// SignFile wraps package bytes in a signed package file (modulekit.SignFile): the
// package, its digest, an Ed25519 signature over the exact bytes and the key id.
func SignFile(priv ed25519.PrivateKey, pkg []byte) ([]byte, error) {
	return modulekit.SignFile(priv, pkg)
}
