package store

import (
	"context"
	"strings"
	"testing"

	"github.com/Docker-Hunterpedia/serverdock/internal/secretbox"
)

// The "missing key but ciphertexts exist" check must run on every schema
// version a database can be at: before M2's tables exist, after each migration,
// and from the M1 fixture.
func TestCiphertextsExistOnEverySchemaVersion(t *testing.T) {
	ctx := context.Background()
	for n := 1; n <= len(builtin); n++ {
		dir := t.TempDir()
		st, err := openWith(ctx, dir, Options{Migrations: builtin[:n]})
		if err != nil {
			t.Fatalf("schema v%d: %v", n, err)
		}
		if ok, err := secretbox.CiphertextsExist(ctx, st.R, secretbox.Columns()); err != nil || ok {
			t.Errorf("schema v%d: %v %v", n, ok, err)
		}
		st.Close()
	}
	dir := copyFixture(t, "v2.db")
	st, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if ok, err := secretbox.CiphertextsExist(ctx, st.R, secretbox.Columns()); err != nil || ok {
		t.Errorf("v2 fixture: %v %v", ok, err)
	}
}

// A migration that adds a column which looks like it holds a secret must also
// list it in secretbox.Columns(), or `admin reset` would not clear it and the
// missing-key check would not see it.
func TestSecretLookingColumnsAreListedInSecretboxColumns(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	listed := map[string]bool{}
	for _, c := range secretbox.Columns() {
		listed[c.Table+"."+c.Column] = true
	}
	rows, err := st.R.QueryContext(ctx, `SELECT m.name, p.name FROM sqlite_master m, pragma_table_info(m.name) p WHERE m.type = 'table'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var table, col string
		if err := rows.Scan(&table, &col); err != nil {
			t.Fatal(err)
		}
		for _, suffix := range []string{"_seed", "_secret", "_ciphertext"} {
			if strings.HasSuffix(col, suffix) && !listed[table+"."+col] {
				t.Errorf("%s.%s looks like it holds a secret but is not in secretbox.Columns()", table, col)
			}
		}
	}
}
