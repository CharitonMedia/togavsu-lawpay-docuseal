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
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"embed"
	"errors"
	"io"
	"io/fs"
	"sort"
	"strings"
)

// ID is the module id from the manifest.
const ID = "lawpay-docuseal"

// ManifestFile is the signed manifest's name inside the package.
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

// packageFileMode is the mode every package entry carries (0644), as the core writes them.
const packageFileMode = 0o644

// Archive writes the unsigned package: a gzip tar of the package files in path order, the
// format the core's modules.Archive writes. edit, when set, changes the files first (tests
// use it to make tampered and refused variants).
func Archive(edit func(map[string][]byte)) ([]byte, error) {
	files, err := Files()
	if err != nil {
		return nil, err
	}
	if edit != nil {
		edit(files)
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, n := range names {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: packageFileMode, Size: int64(len(files[n])), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(files[n]); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// SignedPackage is the marketplace's signed package file (marketplace.SignedPackage in the
// core): the package bytes, their digest, an Ed25519 signature over the exact bytes and the
// signing key's id.
type SignedPackage struct {
	ListingID     string         `json:"listing_id"`
	Version       string         `json:"version"`
	Immutable     bool           `json:"immutable"`
	Manifest      map[string]any `json:"manifest"`
	Digest        string         `json:"digest"`
	Signature     string         `json:"signature"`
	KeyID         string         `json:"key_id"`
	PackageBase64 string         `json:"package_base64"`
}

// Build writes the package signed with priv as a signed package file, which the core
// installs from a local upload and the marketplace publishes.
func Build(priv ed25519.PrivateKey, edit func(map[string][]byte)) ([]byte, error) {
	pkg, err := Archive(edit)
	if err != nil {
		return nil, err
	}
	return SignFile(priv, pkg)
}

// SignFile wraps package bytes in a signed package file, signed through the same path as
// the core's marketplace.Sign, Digest and KeyID.
func SignFile(priv ed25519.PrivateKey, pkg []byte) ([]byte, error) {
	id, version, err := manifestIdentity(pkg)
	if err != nil {
		return nil, err
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("lawpay-docuseal: the signing key is not Ed25519")
	}
	return json.Marshal(SignedPackage{ListingID: id, Version: version, Digest: Digest(pkg),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, pkg)), KeyID: KeyID(pub),
		PackageBase64: base64.StdEncoding.EncodeToString(pkg)})
}

// Digest is the package digest in the marketplace's form, sha256:<hex>.
func Digest(pkg []byte) string {
	sum := sha256.Sum256(pkg)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// keyIDHexLen is the fingerprint length the core uses: the first 16 bytes of SHA-256, as hex.
const keyIDHexLen = sha256.Size

// KeyID names a public key as the marketplace and the core do.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "ed25519-" + hex.EncodeToString(sum[:])[:keyIDHexLen]
}

// VerifyFile checks a signed package file against pub: the digest over the bytes and the
// signature, as the core does at install.
func VerifyFile(file []byte, pub ed25519.PublicKey) ([]byte, error) {
	var sp SignedPackage
	if err := json.Unmarshal(file, &sp); err != nil {
		return nil, err
	}
	pkg, err := base64.StdEncoding.DecodeString(sp.PackageBase64)
	if err != nil {
		return nil, err
	}
	if sp.Digest != Digest(pkg) {
		return nil, errors.New("digest_mismatch")
	}
	if sp.KeyID != KeyID(pub) {
		return nil, errors.New("untrusted_key")
	}
	sig, err := base64.StdEncoding.DecodeString(sp.Signature)
	if err != nil || !ed25519.Verify(pub, pkg, sig) {
		return nil, errors.New("signature_invalid")
	}
	return pkg, nil
}

// manifestIdentity reads id and version from the manifest inside pkg. The manifest is
// block YAML; both keys are top-level scalars.
func manifestIdentity(pkg []byte) (string, string, error) {
	files, err := Unpack(pkg)
	if err != nil {
		return "", "", err
	}
	body, ok := files[ManifestFile]
	if !ok {
		return "", "", errors.New("lawpay-docuseal: the package has no " + ManifestFile)
	}
	var id, version string
	for _, line := range strings.Split(string(body), "\n") {
		k, v, found := strings.Cut(line, ":")
		if !found || strings.HasPrefix(k, " ") {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"`)
		switch k {
		case "id":
			id = v
		case "version":
			version = v
		}
	}
	if id == "" || version == "" {
		return "", "", errors.New("lawpay-docuseal: the manifest names no id or version")
	}
	return id, version, nil
}

// Unpack reads a package back into its files.
func Unpack(pkg []byte) (map[string][]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(pkg))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	out := map[string][]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		var b bytes.Buffer
		if _, err := b.ReadFrom(tr); err != nil {
			return nil, err
		}
		out[h.Name] = b.Bytes()
	}
}
