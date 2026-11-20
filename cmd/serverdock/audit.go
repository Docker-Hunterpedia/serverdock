package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/Docker-Hunterpedia/serverdock/internal/audit"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

// auditMain implements `serverdock audit verify --data DIR`. It opens the
// database read-only (it works next to a running server, even as another user,
// and creates no files). Exit codes: 0 chain valid, 1 chain broken, 2 could not
// check.
func auditMain(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "verify" {
		fmt.Fprintln(stderr, "usage: serverdock audit verify [--data DIR]")
		return 2
	}
	fs := flag.NewFlagSet("audit verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := fs.String("data", "/var/lib/serverdock", "data directory")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "serverdock audit verify: unexpected arguments: %v\n", fs.Args())
		return 2
	}
	ro, err := store.OpenReadOnly(*data, store.ReadOnlyOptions{})
	if err != nil {
		fmt.Fprintln(stderr, "serverdock audit verify:", err)
		return 2
	}
	defer ro.Close()
	var n int
	if err := ro.DB.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='audit_log'`).Scan(&n); err != nil || n == 0 {
		if err == nil || errors.Is(err, sql.ErrNoRows) {
			err = fmt.Errorf("no audit log in this database yet (schema %s); start the server once to migrate it", ro.Schema)
		}
		fmt.Fprintln(stderr, "serverdock audit verify:", err)
		return 2
	}
	res, err := audit.Verify(context.Background(), ro.DB)
	if err != nil {
		fmt.Fprintln(stderr, "serverdock audit verify:", err)
		return 2
	}
	if !res.OK {
		fmt.Fprintf(stdout, "BROKEN: the audit chain does not verify at row id %d (%d rows)\n", res.BrokenID, res.Rows)
		return 1
	}
	head := "(empty)"
	if res.Rows > 0 {
		head = hex.EncodeToString(res.Head)
	}
	fmt.Fprintf(stdout, "ok: %d rows, head %s\n", res.Rows, head)
	return 0
}
