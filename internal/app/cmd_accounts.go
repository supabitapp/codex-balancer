package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"text/tabwriter"
	"time"
)

const accountsHelp = `Manage the account pool.

Usage:
  codex-balancer accounts add                 Sign in to ChatGPT and pool the account
  codex-balancer accounts add --device-auth   Sign in with a code on another device
  codex-balancer accounts list                Show pooled accounts
  codex-balancer accounts mode <account> <mode> Set routing to normal or priority
  codex-balancer accounts rm <email>          Drop an account

Flags:
  -state string      state database (default %s)
  -device-auth       sign in with a one-time code, add only
  -json              machine-readable output, list only
`

func printAccountsHelp(w io.Writer) {
	fmt.Fprintf(w, accountsHelp, defaultStatePath())
}

func accountsCmd(args []string) error {
	if len(args) == 0 {
		printAccountsHelp(os.Stderr)
		return errors.New("no subcommand given")
	}

	fs := flag.NewFlagSet("accounts", flag.ContinueOnError)
	fs.Usage = func() { printAccountsHelp(os.Stderr) }
	path := fs.String("state", defaultStatePath(), "state database")
	deviceAuth := fs.Bool("device-auth", false, "sign in with a one-time code")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	store, err := openStateStore(*path)
	if err != nil {
		return err
	}
	defer store.Close()
	pool, err := loadPool(store)
	if err != nil {
		return err
	}

	switch args[0] {
	case "add":
		return addAccount(pool, *deviceAuth)
	case "list":
		return listAccounts(pool, *asJSON)
	case "mode":
		if fs.NArg() != 2 {
			return errors.New("accounts mode needs an account and one of: normal, priority")
		}
		account, err := pool.resolve(fs.Arg(0))
		if err != nil {
			return err
		}
		mode := routingMode(fs.Arg(1))
		if err := pool.setRoutingMode(account, mode); err != nil {
			return err
		}
		fmt.Printf("set %s routing mode to %s\n", describe(account), mode)
		return nil
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

func addAccount(pool *Pool, deviceAuth bool) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var account *Account
	var err error
	if deviceAuth {
		account, err = loginWithDeviceCode(ctx, http.DefaultClient, os.Stderr, authBaseURL)
	} else {
		account, err = login(ctx, http.DefaultClient)
	}
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
			candidate := a.routingCandidate()
			out = append(out, map[string]any{
				"id":           a.id(),
				"email":        a.email(),
				"plan":         a.plan(),
				"paused":       candidate.paused,
				"routing_mode": candidate.mode,
				"token":        tokenStatus(a),
				"expiry":       a.expires(),
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
	fmt.Fprintln(w, "EMAIL\tPLAN\tROUTING\tTOKEN")
	for _, a := range accounts {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", label(a), a.plan(), routing(a), tokenStatus(a))
	}
	return w.Flush()
}

func routing(a *Account) string {
	candidate := a.routingCandidate()
	if candidate.paused {
		return "paused"
	}
	if candidate.mode == routingModePriority {
		return string(routingModePriority)
	}
	return string(routingModeNormal)
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

func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return home
}
