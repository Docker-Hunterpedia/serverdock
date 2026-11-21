package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/audit"
	"github.com/Docker-Hunterpedia/serverdock/internal/auth"
	"github.com/Docker-Hunterpedia/serverdock/internal/datadir"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

// adminMain implements `serverdock admin reset [--data DIR] [--yes]`, the console
// recovery for a lost password or authenticator. Exit codes: 0 done, 1 refused or
// failed, 2 usage.
func adminMain(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return adminReset(args, stdin, stdinIsTerminal(), os.Geteuid(), stdout, stderr)
}

func stdinIsTerminal() bool {
	st, err := os.Stdin.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// adminResetMain is the entry point of the reset process. The umask is set before
// anything is created (the migration backup, the master key, SQLite's sidecars).
func adminResetMain(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	restrictUmask()
	return adminMain(args, stdin, stdout, stderr)
}

func adminReset(args []string, stdin io.Reader, tty bool, euid int, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "reset" {
		fmt.Fprintln(stderr, "usage: serverdock admin reset [--data DIR] [--yes]")
		return 2
	}
	fs := flag.NewFlagSet("admin reset", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := fs.String("data", "/var/lib/serverdock", "data directory")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "serverdock admin reset: unexpected arguments: %v\n", fs.Args())
		return 2
	}
	if !*yes && !tty {
		fmt.Fprintln(stderr, "serverdock admin reset: this is not a terminal, so it cannot ask for confirmation; run it from a terminal, or pass --yes")
		return 2
	}

	// Owner only. Running as root against a data directory somebody else owns is
	// refused outright (no privilege dropping): it could leave root-owned files in
	// DATA that stop the service user's server from starting.
	st, err := os.Stat(*data)
	if err != nil {
		fmt.Fprintf(stderr, "serverdock admin reset: no data directory at %s: %v\n", *data, err)
		return 1
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && euid == 0 && sys.Uid != 0 {
		owner := strconv.Itoa(int(sys.Uid))
		who := "'#" + owner + "'"
		if u, err := user.LookupId(owner); err == nil {
			who = u.Username
		}
		fmt.Fprintf(stderr, "serverdock admin reset: %s is owned by %s; run: sudo -u %s serverdock admin reset --data %s\n", *data, who, who, *data)
		return 1
	}
	if err := datadir.Prepare(*data); err != nil {
		fmt.Fprintln(stderr, "serverdock admin reset:", err)
		return 1
	}

	if !*yes {
		fmt.Fprint(stdout, "This clears the admin password, two-factor and recovery codes, signs everyone out and revokes every API token.\nType reset to continue: ")
		line, _ := bufio.NewReader(stdin).ReadString('\n')
		if strings.TrimSpace(line) != "reset" {
			fmt.Fprintln(stderr, "serverdock admin reset: not confirmed; nothing was changed")
			return 1
		}
	}

	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	// store.Open takes the data-directory lock (a running server holds it) and runs
	// pending migrations, with the pre-migration backup, exactly as a server start would.
	stor, err := store.Open(ctx, *data)
	if err != nil {
		if errors.Is(err, store.ErrLocked) {
			fmt.Fprintln(stderr, "serverdock admin reset: ServerDock is running; stop it first (systemctl stop serverdock, or stop its container)")
		} else {
			fmt.Fprintln(stderr, "serverdock admin reset:", err)
		}
		return 1
	}
	defer stor.Close()
	auditLog, err := audit.New(stor, log)
	if err != nil {
		fmt.Fprintln(stderr, "serverdock admin reset: audit log:", err)
		return 1
	}
	defer func() { _ = auditLog.Flush(context.WithoutCancel(ctx)) }()

	// A registry holding only auth.admin_reset (the server's never has it), run
	// through the one-time SystemInvoker: intent and outcome rows with actor system.
	reg := actions.New(auditLog, log)
	as := auth.NewStore(stor, nil, nil, auth.Options{Log: log})
	auth.RegisterAdminReset(reg, as)
	res, err := reg.SystemInvoker().Invoke(ctx, "auth.admin_reset", json.RawMessage(`{}`), actions.Options{})
	if err != nil {
		fmt.Fprintln(stderr, "serverdock admin reset: failed:", err)
		return 1
	}
	out, _ := res.Out.(auth.ResetResult)
	// A stale setup token file from before the reset is of no use (its row is gone).
	if err := os.Remove(filepath.Join(*data, auth.SetupTokenFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(stderr, "serverdock admin reset: could not remove the stale setup token file: %v\n", err)
	}
	fmt.Fprintf(stdout, "Reset done: %d sessions deleted, %d pending sign-ins deleted, %d API tokens revoked", out.SessionsDeleted, out.PendingDeleted, out.TokensRevoked)
	if out.KeyCreated {
		fmt.Fprint(stdout, ", a new master key created")
	}
	fmt.Fprintf(stdout, ". Audit row %d (intent).\n", res.AuditIntentID)
	if !res.AuditOutcomeRecorded {
		fmt.Fprintln(stdout, "Warning: the audit outcome row could not be written.")
	}
	fmt.Fprintf(stdout, "Start the server; it will write a new setup token to %s/setup-token\n", strings.TrimSuffix(*data, "/"))
	return 0
}
