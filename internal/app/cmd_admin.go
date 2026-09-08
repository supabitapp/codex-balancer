package app

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/charmbracelet/x/term"
)

const adminHelp = `Manage browser admin access.

Usage:
  codex-balancer admin password [-state path]  Set or reset the password
  codex-balancer admin disable [-state path]   Disable admin access

Password entry is hidden and requires a terminal. No old password is needed.
The salted password hash is stored in the state database. Setting a new
password or disabling access invalidates existing admin sessions.
`

func adminCmd(args []string) error {
	help := func() { fmt.Fprint(os.Stdout, adminHelp) }
	if len(args) == 0 {
		help()
		return errors.New("no subcommand given")
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		help()
		return nil
	}
	fs := flag.NewFlagSet("admin", flag.ContinueOnError)
	fs.Usage = help
	path := fs.String("state", defaultStatePath(), "state database")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || (args[0] != "password" && args[0] != "disable") {
		return errors.New("use admin password or admin disable; passwords are entered interactively")
	}
	if args[0] == "password" && !term.IsTerminal(os.Stdin.Fd()) {
		return errors.New("admin password requires an interactive terminal (use ssh -t for a remote server)")
	}
	store, err := openStateStore(*path)
	if err != nil {
		return err
	}
	defer store.Close()
	if args[0] == "disable" {
		if err := store.raw.SetAdminPasswordHash(""); err != nil {
			return err
		}
		fmt.Println("Admin access disabled. Existing admin sessions are invalid.")
		return nil
	}
	return changeAdminPassword(store, func() ([]byte, error) { return term.ReadPassword(os.Stdin.Fd()) }, os.Stderr)
}

func changeAdminPassword(store *StateStore, read func() ([]byte, error), out io.Writer) error {
	fmt.Fprint(out, "New admin password: ")
	password, err := read()
	fmt.Fprintln(out)
	if err != nil {
		return err
	}
	defer clear(password)
	fmt.Fprint(out, "Confirm password: ")
	confirmation, err := read()
	fmt.Fprintln(out)
	if err != nil {
		return err
	}
	defer clear(confirmation)
	if !bytes.Equal(password, confirmation) {
		return errors.New("passwords do not match")
	}
	hash, err := hashAdminPassword(password)
	if err != nil {
		return err
	}
	if err := store.raw.SetAdminPasswordHash(hash); err != nil {
		return err
	}
	fmt.Fprintln(out, "Admin password saved. Existing admin sessions are invalid.")
	return nil
}
