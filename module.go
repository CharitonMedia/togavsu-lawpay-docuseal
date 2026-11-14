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
package lawpaydocuseal

import (
	"embed"
	"errors"
	"io/fs"
	"strings"
)

// ID is the module id from the manifest.
const ID = "lawpay-docuseal"

// ManifestFile is the manifest's name inside the package.
const ManifestFile = "togavsu-module.yaml"

//go:embed package
var packageFS embed.FS

// Files returns the package files (manifest and migrations) by package path.
func Files() (map[string][]byte, error) {
	out := map[string][]byte{}
	err := fs.WalkDir(packageFS, "package", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := packageFS.ReadFile(p)
		if err != nil {
			return err
		}
		out[strings.TrimPrefix(p, "package/")] = b
		return nil
	})
	return out, err
}

// Packager turns the package files into the signed package file the core installs. The
// core owns the package format, the digest and the signature, and the module must not copy
// or reimplement them: a copy would drift from what the core verifies at install. This
// interface is the seam where a public packaging and signing package (or CLI) from the core
// plugs in once one exists.
type Packager interface {
	// Package archives files and signs the archive with the Ed25519 key at keyPath,
	// returning the signed package file.
	Package(files map[string][]byte, keyPath string) ([]byte, error)
}

// ErrPackagerPending is what the stub answers: packaging waits on the core's public
// packaging and signing package or CLI.
var ErrPackagerPending = errors.New("lawpay-docuseal: packaging and signing wait on a public packaging package or CLI from the core; " +
	"this build has no packager")

// PendingPackager is the stub Packager until the core publishes one.
type PendingPackager struct{}

// Package always answers ErrPackagerPending.
func (PendingPackager) Package(map[string][]byte, string) ([]byte, error) { return nil, ErrPackagerPending }

// Build packages and signs this module's files with p.
func Build(p Packager, keyPath string, edit func(map[string][]byte)) ([]byte, error) {
	if p == nil {
		p = PendingPackager{}
	}
	files, err := Files()
	if err != nil {
		return nil, err
	}
	if edit != nil {
		edit(files)
	}
	return p.Package(files, keyPath)
}
