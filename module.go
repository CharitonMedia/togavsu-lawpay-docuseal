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
// Packaging and signing go through the core's public module kit (pkg/modulekit): Build
// holds back the parts package/pending.yaml lists that the pinned core refuses, then checks
// and signs the rest with the code the core runs at install.
package lawpaydocuseal

import (
	"crypto/ed25519"
	"embed"
	"io/fs"
	"strconv"
	"strings"

	"github.com/CharitonMedia/togavsu/pkg/modulekit"
)

// ID is the module id from the manifest.
const ID = "lawpay-docuseal"

// ManifestFile is the manifest's name inside the package.
const ManifestFile = "togavsu-module.yaml"

//go:embed package
var packageFS embed.FS

// Files returns the package files (manifest, migrations and pending.yaml) by package
// path, read with the kit's rules (names starting with "." or "_" left out), so the
// embedded package and the package directory on disk ship the same bytes.
func Files() (map[string][]byte, error) {
	dir, err := fs.Sub(packageFS, "package")
	if err != nil {
		return nil, err
	}
	return modulekit.Files(dir)
}

// Installable returns the package files the pinned core accepts (modulekit.Installable):
// the package without package/pending.yaml and without each part it lists that the core
// still refuses, and the parts it held back. A part the core accepts stays in, so each
// one returns on its own when its core item lands.
func Installable() (map[string][]byte, []modulekit.Held, error) {
	files, err := Files()
	if err != nil {
		return nil, nil, err
	}
	return modulekit.Installable(files)
}

// Build writes the signed package file the admin installs: Installable's files, checked,
// archived and signed with priv (modulekit.BuildInstallable). It returns the parts it held
// back.
func Build(priv ed25519.PrivateKey) ([]byte, []modulekit.Held, error) {
	files, err := Files()
	if err != nil {
		return nil, nil, err
	}
	return modulekit.BuildInstallable(priv, files)
}

// settingDefault is the default the manifest declares for a setting key, or "" when it
// declares none.
func settingDefault(key string) string {
	raw, err := packageFS.ReadFile("package/" + ManifestFile)
	if err != nil {
		return ""
	}
	in := false
	for _, line := range strings.Split(string(raw), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "- key: ") {
			in = t == "- key: "+key
			continue
		}
		if v, ok := strings.CutPrefix(t, "default: "); ok && in {
			if u, err := strconv.Unquote(v); err == nil {
				return u
			}
			return v
		}
	}
	return ""
}
