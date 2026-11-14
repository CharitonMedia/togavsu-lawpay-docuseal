// Command lawpay-docuseal runs the LawPay and DocuSeal module, or packages it.
//
//	lawpay-docuseal serve -core http://core:8080 -secrets /run/secrets -db-host postgres:5432 -db-name togavsu
//	lawpay-docuseal package -key signing.key -out lawpay-docuseal.signed.json [-dir package]
//
// package holds back the parts package/pending.yaml lists that the pinned core refuses,
// prints one line for each with the core item it waits on, and signs the rest.
//
// Secrets come from the Docker secrets the core hands the supervisor: togavsu_api_token,
// togavsu_webhook_secret, togavsu_db_role, togavsu_db_password and togavsu_db_schema.
// LawPay and DocuSeal keys are module settings an admin enters at runtime; the module
// reads them from the core and never from its environment.
package main

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	lawpaydocuseal "github.com/CharitonMedia/togavsu-lawpay-docuseal"
	"github.com/CharitonMedia/togavsu/pkg/modulekit"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	log.SetFlags(zero)
	if len(os.Args) < flagsFrom {
		log.Fatal("usage: lawpay-docuseal serve|package [flags]")
	}
	switch os.Args[commandArg] {
	case "serve":
		serve(os.Args[flagsFrom:])
	case "package":
		pack(os.Args[flagsFrom:])
	default:
		log.Fatalf("unknown command %q", os.Args[commandArg])
	}
}

func secret(dir, name string) string {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		log.Fatalf("secret %s: %v", name, err)
	}
	return strings.TrimSpace(string(b))
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", ":8080", "address to listen on")
	core := fs.String("core", "http://core:8080", "core base URL inside the stack")
	dir := fs.String("secrets", "/run/secrets", "directory of Docker secrets")
	host := fs.String("db-host", "postgres:5432", "Postgres host:port")
	name := fs.String("db-name", "togavsu", "database name")
	// The PRD's webhook rule: a core delivery more than five minutes off is refused.
	skew := fs.String("webhook-skew", "5m", "allowed core webhook clock skew")
	_ = fs.Parse(args)
	skewDur, err := time.ParseDuration(*skew)
	if err != nil {
		log.Fatal(err)
	}
	q := url.Values{"search_path": {secret(*dir, "togavsu_db_schema")}}
	u := url.URL{Scheme: "postgres", User: url.UserPassword(secret(*dir, "togavsu_db_role"), secret(*dir, "togavsu_db_password")),
		Host: *host, Path: "/" + *name, RawQuery: q.Encode()}
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		log.Fatal(err)
	}
	srv := lawpaydocuseal.New(lawpaydocuseal.Config{Core: *core, Token: secret(*dir, "togavsu_api_token"),
		WebhookSecret: secret(*dir, "togavsu_webhook_secret"), DB: db, SkewSeconds: int(skewDur.Seconds())})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go srv.Run(ctx)
	hs := &http.Server{Addr: *listen, Handler: srv.Handler()}
	go func() {
		<-ctx.Done()
		_ = hs.Shutdown(context.Background())
	}()
	log.Printf("lawpay-docuseal listening on %s", *listen)
	if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func pack(args []string) {
	fs := flag.NewFlagSet("package", flag.ExitOnError)
	keyFile := fs.String("key", "", "file holding a base64 Ed25519 seed")
	out := fs.String("out", "lawpay-docuseal.signed.json", "signed package file to write")
	dir := fs.String("dir", "", "package directory to build instead of the embedded package")
	_ = fs.Parse(args)
	b, err := buildSigned(*keyFile, *dir)
	if err != nil {
		log.Fatal(err)
	}
	for _, h := range b.held {
		fmt.Fprintln(os.Stderr, h.String())
	}
	for _, e := range b.unused {
		fmt.Fprintf(os.Stderr, "pending %s (%s) holds nothing back: the core accepts it or the package no longer has it\n", e.Part(), e.Core)
	}
	if err := os.WriteFile(*out, b.signed, signedFileMode); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("wrote %s, signed by key %s\npublic key (add it as a trusted signing key, public_key_pem):\n%s",
		*out, modulekit.KeyID(b.pub), modulekit.PublicKeyPEM(b.pub))
}

// built is a signed package file, its signer's public key, the parts the pinned core
// refuses that were held back (package/pending.yaml), and the pending entries that held
// nothing back.
type built struct {
	signed []byte
	pub    ed25519.PublicKey
	held   []modulekit.Held
	unused []modulekit.Pending
}

// buildSigned reads the signing key and the package files (the embedded package, or dir
// when set), holds back the pending parts the core refuses and signs the rest
// (modulekit.BuildInstallable), as togavsu-module package -installable does. It refuses,
// listing every refusal, a package the core would refuse once those parts are out.
func buildSigned(keyFile, dir string) (built, error) {
	raw, err := os.ReadFile(keyFile)
	if err != nil {
		return built{}, err
	}
	priv, err := modulekit.ParseSeed(string(raw))
	if err != nil {
		return built{}, err
	}
	var files map[string][]byte
	if dir != "" {
		files, err = modulekit.ReadDir(dir)
	} else {
		files, err = lawpaydocuseal.Files()
	}
	if err != nil {
		return built{}, err
	}
	signed, ip, err := modulekit.BuildInstallable(priv, files)
	if err != nil {
		return built{}, refused(err)
	}
	return built{signed: signed, pub: priv.Public().(ed25519.PublicKey), held: ip.Held, unused: ip.Unused}, nil
}

// refused is a build error with every refusal on its own line.
func refused(err error) error {
	var refs modulekit.Refusals
	if !errors.As(err, &refs) {
		return fmt.Errorf("package refused (%s): %w", modulekit.Code(err), err)
	}
	lines := make([]string, zero, len(refs))
	for _, r := range refs {
		lines = append(lines, fmt.Sprintf("%s: %s: %s", r.Where(), r.Code, r.Message))
	}
	return fmt.Errorf("package refused (%s):\n%s", modulekit.Code(err), strings.Join(lines, "\n"))
}
