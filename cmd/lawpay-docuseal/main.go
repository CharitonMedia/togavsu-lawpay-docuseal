// Command lawpay-docuseal runs the LawPay and DocuSeal module, or packages it.
//
//	lawpay-docuseal serve -core http://core:8080 -secrets /run/secrets -db-host postgres:5432 -db-name togavsu
//	lawpay-docuseal package -key signing.key -out lawpay-docuseal.signed.json [-dir package]
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
	signed, pub, err := buildSigned(*keyFile, *dir)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*out, signed, signedFileMode); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("wrote %s, signed by key %s\npublic key (add it as a trusted signing key, public_key_pem):\n%s",
		*out, modulekit.KeyID(pub), modulekit.PublicKeyPEM(pub))
}

// buildSigned reads the signing key and the package files (the embedded package, or dir
// when set), refuses what the core's install check refuses, as togavsu-module package
// does, and returns the signed package file and the signer's public key.
func buildSigned(keyFile, dir string) ([]byte, ed25519.PublicKey, error) {
	raw, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, nil, err
	}
	priv, err := modulekit.ParseSeed(string(raw))
	if err != nil {
		return nil, nil, err
	}
	var files map[string][]byte
	if dir != "" {
		files, err = modulekit.ReadDir(dir)
	} else {
		files, err = lawpaydocuseal.Files()
	}
	if err != nil {
		return nil, nil, err
	}
	if _, _, err := modulekit.Check(files); err != nil {
		return nil, nil, fmt.Errorf("package refused (%s): %w", modulekit.Code(err), err)
	}
	signed, err := modulekit.Build(priv, files)
	if err != nil {
		return nil, nil, err
	}
	return signed, priv.Public().(ed25519.PublicKey), nil
}
