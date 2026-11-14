package lawpaydocuseal

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/CharitonMedia/togavsu-lawpay-docuseal/fake"
)

// Deleting a core record the module's rows point at fails, and the error names the
// module table (ON DELETE RESTRICT).
func TestPGRestrictForeignKeys(t *testing.T) {
	e := newEnv(t)
	e.staffCharge("operating", 5000, fake.CardToken)
	_, err := e.db.Exec(`DELETE FROM core.matters WHERE id = $1`, e.matter.ID)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "23503" || !strings.Contains(pe.Detail+pe.Message, "lawpay_charges") {
		t.Fatalf("delete: %v", err)
	}
}

// Every foreign key to a core table is ON DELETE RESTRICT, and the published views exist.
func TestPGMigrationsShape(t *testing.T) {
	e := newEnv(t)
	rows, err := e.db.Query(`SELECT c.conname, c.confdeltype, n.nspname FROM pg_constraint c
		JOIN pg_class t ON t.oid = c.confrelid JOIN pg_namespace n ON n.oid = t.relnamespace
		WHERE c.contype = 'f' AND c.conrelid::regclass::text NOT LIKE 'core.%' AND c.connamespace = (SELECT oid FROM pg_namespace WHERE nspname = current_schema())`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	core := 0
	for rows.Next() {
		var name, del, ns string
		if err := rows.Scan(&name, &del, &ns); err != nil {
			t.Fatal(err)
		}
		if del != "r" {
			t.Errorf("%s: on delete %s, want restrict", name, del)
		}
		if ns == "core" {
			core++
		}
	}
	if core == 0 {
		t.Fatal("no foreign keys to core found")
	}
	for _, v := range []string{"invoice_payability", "matter_payability", "signature_requests"} {
		if n := e.count(`SELECT count(*) FROM information_schema.views WHERE table_schema = current_schema() AND table_name = $1`, v); n != 1 {
			t.Errorf("view %s missing", v)
		}
	}
}

var statementRe = regexp.MustCompile(`(?s)^(CREATE TABLE|CREATE INDEX|CREATE UNIQUE INDEX|CREATE VIEW) `)

// Migrations hold only CREATE TABLE, INDEX and VIEW statements.
func TestMigrationsOnlyCreate(t *testing.T) {
	files, err := Files()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for name, b := range files {
		if !strings.HasPrefix(name, "migrations/") {
			continue
		}
		var lines []string
		for _, l := range strings.Split(string(b), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(l), "--") {
				lines = append(lines, l)
			}
		}
		for _, stmt := range strings.Split(strings.Join(lines, "\n"), ";") {
			stmt = strings.TrimSpace(stmt)
			if stmt == "" {
				continue
			}
			n++
			if !statementRe.MatchString(stmt) {
				t.Errorf("%s: statement not allowed: %.60s", name, stmt)
			}
			if strings.Contains(stmt, "REFERENCES core.") && strings.Count(stmt, "REFERENCES") != strings.Count(stmt, "ON DELETE RESTRICT") {
				t.Errorf("%s: a foreign key without ON DELETE RESTRICT in %.60s", name, stmt)
			}
		}
	}
	if n == 0 {
		t.Fatal("no migrations")
	}
}
