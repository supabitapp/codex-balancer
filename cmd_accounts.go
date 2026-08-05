package main

import (
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
  codex-balancer accounts add [file]   Import a Codex auth.json (default ~/.codex/auth.json, "-" for stdin)
  codex-balancer accounts list         Show pooled accounts
  codex-balancer accounts rm <id>      Drop an account

Flags:
  -accounts string   account pool file (default %s)
  -json              machine-readable output, list only
`

func accountsCmd(args []string) error {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, accountsHelp, defaultAccountsPath())
		return errors.New("no subcommand given")
	}

	fs := flag.NewFlagSet("accounts", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintf(os.Stderr, accountsHelp, defaultAccountsPath()) }
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
		return addAccount(pool, fs.Arg(0))
	case "list":
		return listAccounts(pool, *asJSON)
	case "rm":
		id := fs.Arg(0)
		if id == "" {
			return errors.New("accounts rm needs an account id")
		}
		if err := pool.remove(id); err != nil {
			return err
		}
		fmt.Printf("removed %s\n", id)
		return nil
	case "help", "-h", "--help":
		fmt.Printf(accountsHelp, defaultAccountsPath())
		return nil
	}
	fmt.Fprintf(os.Stderr, accountsHelp, defaultAccountsPath())
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

func addAccount(pool *Pool, source string) error {
	if source == "" {
		source = filepath.Join(homeDir(), ".codex", "auth.json")
	}

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
		return fmt.Errorf("parse %s: %w", source, err)
	}
	if file.Tokens.RefreshToken == "" {
		return fmt.Errorf("%s carries no ChatGPT refresh token; log in with ChatGPT first", source)
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
	fmt.Printf("added %s (%s, %s)\n", account.ID(), account.Email(), account.Plan())
	return nil
}

func listAccounts(pool *Pool, asJSON bool) error {
	accounts := pool.sorted()
	if asJSON {
		out := make([]map[string]any, 0, len(accounts))
		for _, a := range accounts {
			a.mu.Lock()
			out = append(out, map[string]any{
				"id":           a.ID(),
				"email":        a.Email(),
				"plan":         a.Plan(),
				"used_percent": a.pressure,
				"cooldown":     a.cooldown,
				"reauth":       a.dead,
			})
			a.mu.Unlock()
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
	fmt.Fprintln(w, "ID\tEMAIL\tPLAN")
	for _, a := range accounts {
		fmt.Fprintf(w, "%s\t%s\t%s\n", a.ID(), a.Email(), a.Plan())
	}
	return w.Flush()
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
