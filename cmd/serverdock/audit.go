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
	return auditVerifyCmd(args, stdout, stderr, store.OpenReadOnly)
}

type openRO func(dir string, opts store.ReadOnlyOptions) (*store.ReadOnly, error)

// checkOnce opens the database and walks the chain.
func checkOnce(dir string, open openRO) (res audit.VerifyResult, autoImmutable bool, err error) {
	ro, err := open(dir, store.ReadOnlyOptions{})
	if err != nil {
		return res, false, err
	}
	defer ro.Close()
	var n int
	if qerr := ro.DB.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='audit_log'`).Scan(&n); qerr != nil || n == 0 {
		if qerr == nil || errors.Is(qerr, sql.ErrNoRows) {
			qerr = fmt.Errorf("no audit log in this database yet (schema %s); start the server once to migrate it", ro.Schema)
		}
		return res, false, qerr
	}
	res, err = audit.Verify(context.Background(), ro.DB)
	return res, ro.AutoImmutable, err
}

func auditVerifyCmd(args []string, stdout, stderr io.Writer, open openRO) int {
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
	res, auto, err := checkOnce(*data, open)
	if err == nil && !res.OK && auto {
		// The database was opened immutable because no server was running; one may
		// have started (and written) between that check and the read, which can
		// look like corruption. Look once more before reporting a broken chain.
		res, _, err = checkOnce(*data, open)
	}
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
