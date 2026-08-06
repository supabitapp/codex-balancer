package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"text/tabwriter"
	"time"
)

const accountsHelp = `Manage the account pool.

Usage:
  codex-balancer accounts add          Sign in to ChatGPT in a browser and pool the account
  codex-balancer accounts list         Show pooled accounts
  codex-balancer accounts rm <email>   Drop an account

Flags:
  -accounts string   account pool file (default %s)
  -json              machine-readable output, list only
`

func printAccountsHelp(w io.Writer) {
	fmt.Fprintf(w, accountsHelp, defaultAccountsPath())
}

func accountsCmd(args []string) error {
	if len(args) == 0 {
		printAccountsHelp(os.Stderr)
		return errors.New("no subcommand given")
	}

	fs := flag.NewFlagSet("accounts", flag.ContinueOnError)
	fs.Usage = func() { printAccountsHelp(os.Stderr) }
	path := fs.String("accounts", defaultAccountsPath(), "account pool file")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	pool, err := loadPool(*path)
	if err != nil {
		return err
	}

	switch args[0] {
	case "add":
		return addAccount(pool)
	case "list":
		return listAccounts(pool, *asJSON)
	case "rm":
		who := fs.Arg(0)
		if who == "" {
			return errors.New("accounts rm needs an email")
		}
		account, err := pool.resolve(who)
		if err != nil {
			return err
		}
		if err := pool.remove(account); err != nil {
			return err
		}
		fmt.Printf("removed %s\n", describe(account))
		return nil
	case "help", "-h", "--help":
		printAccountsHelp(os.Stdout)
		return nil
	}
	printAccountsHelp(os.Stderr)
	return fmt.Errorf("unknown subcommand %q", args[0])
}

func addAccount(pool *Pool) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	account, err := login(ctx, http.DefaultClient)
	if err != nil {
		return err
	}
	if err := pool.add(account); err != nil {
		return err
	}
	fmt.Printf("added %s\n", describe(account))
	return nil
}

func describe(a *Account) string {
	if a.email() == "" {
		return a.id()
	}
	return fmt.Sprintf("%s (%s)", a.email(), a.plan())
}

func listAccounts(pool *Pool, asJSON bool) error {
	accounts := pool.sorted()
	if asJSON {
		out := make([]map[string]any, 0, len(accounts))
		for _, a := range accounts {
			out = append(out, map[string]any{
				"id":     a.id(),
				"email":  a.email(),
				"plan":   a.plan(),
				"token":  tokenStatus(a),
				"expiry": a.expires(),
			})
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	if len(accounts) == 0 {
		fmt.Println("no accounts; add one with: codex-balancer accounts add")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "EMAIL\tPLAN\tTOKEN")
	for _, a := range accounts {
		fmt.Fprintf(w, "%s\t%s\t%s\n", cmp.Or(a.email(), a.id()), a.plan(), tokenStatus(a))
	}
	return w.Flush()
}

func tokenStatus(a *Account) string {
	expiry := a.expires()
	switch {
	case expiry.IsZero():
		return "unknown"
	case time.Now().After(expiry):
		return "expired, refreshes on first use"
	default:
		return "valid for " + time.Until(expiry).Round(time.Minute).String()
	}
}

func defaultAccountsPath() string {
	return filepath.Join(homeDir(), ".codex-balancer", "accounts.json")
}

func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return home
}
