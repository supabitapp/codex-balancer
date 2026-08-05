package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"
)

const accountsHelp = `Manage the account pool.

Usage:
  codex-balancer accounts add [file...] Import Codex auth.json files (default ~/.codex/auth.json, "-" for stdin)
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
		return addAccounts(pool, fs.Args())
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

type codexAuthFile struct {
	Tokens struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		AccountID    string `json:"account_id"`
	} `json:"tokens"`
	LastRefresh time.Time `json:"last_refresh"`
}

func addAccounts(pool *Pool, sources []string) error {
	if len(sources) == 0 {
		sources = []string{filepath.Join(homeDir(), ".codex", "auth.json")}
	}
	for _, source := range sources {
		if err := addAccount(pool, source); err != nil {
			return fmt.Errorf("%s: %w", source, err)
		}
	}
	return nil
}

func addAccount(pool *Pool, source string) error {
	var data []byte
	var err error
	if source == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(source)
	}
	if err != nil {
		return err
	}

	var file codexAuthFile
	if err := json.Unmarshal(data, &file); err != nil {
		return err
	}
	if file.Tokens.RefreshToken == "" {
		return errors.New("no ChatGPT refresh token; log in with ChatGPT first")
	}

	account := &Account{
		IDToken:      file.Tokens.IDToken,
		AccessToken:  file.Tokens.AccessToken,
		RefreshToken: file.Tokens.RefreshToken,
		AccountID:    file.Tokens.AccountID,
		LastRefresh:  file.LastRefresh,
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
