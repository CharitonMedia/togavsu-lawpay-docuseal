// Command lawpay-docuseal runs the LawPay and DocuSeal module, or packages it.
//
//	lawpay-docuseal serve -core http://core:8080 -secrets /run/secrets -db-host postgres:5432 -db-name togavsu
//	lawpay-docuseal package -key signing.key -out lawpay-docuseal.signed.json
//
// Secrets come from the Docker secrets the core hands the supervisor: togavsu_api_token,
// togavsu_webhook_secret, togavsu_db_role, togavsu_db_password and togavsu_db_schema.
// LawPay and DocuSeal keys are module settings an admin enters at runtime; the module
// reads them from the core and never from its environment.
package main

import (
	"context"
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
	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		log.Fatal("usage: lawpay-docuseal serve|package [flags]")
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	case "package":
		pack(os.Args[2:])
	default:
		log.Fatalf("unknown command %q", os.Args[1])
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
	keyFile := fs.String("key", "", "file holding the Ed25519 signing key")
	out := fs.String("out", "lawpay-docuseal.signed.json", "signed package file to write")
	_ = fs.Parse(args)
	pkg, err := lawpaydocuseal.Build(lawpaydocuseal.PendingPackager{}, *keyFile, nil)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*out, pkg, 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("wrote %s\n", *out)
}
