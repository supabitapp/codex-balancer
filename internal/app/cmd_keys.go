package app

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

const keysHelp = `Manage client API keys.

Usage:
  codex-balancer keys add <name>    Provision a new API key
  codex-balancer keys list          Show provisioned API keys
  codex-balancer keys rm <name>     Revoke an API key

Flags:
  -state string      state database (default %s)
  -json              machine-readable output, list only
`

func printKeysHelp(w io.Writer) {
	fmt.Fprintf(w, keysHelp, defaultStatePath())
}

func keysCmd(args []string) error {
	if len(args) == 0 {
		printKeysHelp(os.Stderr)
		return errors.New("no subcommand given")
	}

	fs := flag.NewFlagSet("keys", flag.ContinueOnError)
	fs.Usage = func() { printKeysHelp(os.Stderr) }
	path := fs.String("state", defaultStatePath(), "state database")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	switch args[0] {
	case "help", "-h", "--help":
		printKeysHelp(os.Stdout)
		return nil
	case "add", "provision", "list", "rm":
	default:
		printKeysHelp(os.Stderr)
		return fmt.Errorf("unknown subcommand %q", args[0])
	}

	store, err := openStateStore(*path)
	if err != nil {
		return err
	}
	defer store.Close()

	switch args[0] {
	case "add", "provision":
		name, err := keyNameArg(fs)
		if err != nil {
			return err
		}
		secret, err := generateAPIKey()
		if err != nil {
			return err
		}
		if err := store.addAPIKey(storedAPIKey{Name: name, Secret: secret, CreatedAt: time.Now()}); err != nil {
			return fmt.Errorf("provision API key %q: %w", name, err)
		}
		fmt.Println(secret)
		return nil
	case "list":
		if fs.NArg() != 0 {
			return errors.New("keys list takes no arguments")
		}
		return listAPIKeys(store, *asJSON)
	case "rm":
		name, err := keyNameArg(fs)
		if err != nil {
			return err
		}
		revoked, err := store.revokeAPIKey(name, time.Now())
		if err != nil {
			return err
		}
		if !revoked {
			return fmt.Errorf("active API key %q not found", name)
		}
		fmt.Printf("revoked %s\n", name)
		return nil
	}
	return nil
}

func keyNameArg(fs *flag.FlagSet) (string, error) {
	if fs.NArg() != 1 {
		return "", errors.New("API key command needs one name")
	}
	name := fs.Arg(0)
	if name == "" || name != strings.TrimSpace(name) || strings.ContainsAny(name, "\t\r\n") {
		return "", errors.New("API key name cannot start or end with whitespace or contain tabs or newlines")
	}
	return name, nil
}

func generateAPIKey() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate API key: %w", err)
	}
	return "cb_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

func listAPIKeys(store *StateStore, asJSON bool) error {
	keys, err := store.readAPIKeys()
	if err != nil {
		return err
	}
	usage, err := store.apiKeyUsage()
	if err != nil {
		return err
	}
	if asJSON {
		type keyView struct {
			Name      string        `json:"name"`
			CreatedAt time.Time     `json:"created_at"`
			RevokedAt *time.Time    `json:"revoked_at,omitempty"`
			Usage     responseUsage `json:"usage"`
		}
		out := make([]keyView, 0, len(keys))
		for _, key := range keys {
			view := keyView{Name: key.Name, CreatedAt: key.CreatedAt, Usage: usage[key.Name]}
			if !key.RevokedAt.IsZero() {
				revokedAt := key.RevokedAt
				view.RevokedAt = &revokedAt
			}
			out = append(out, view)
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(out)
	}
	if len(keys) == 0 {
		fmt.Println("no API keys; add one with: codex-balancer keys add <name>")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATUS\tINPUT\tCACHED\tOUTPUT\tTOTAL\tCREATED")
	for _, key := range keys {
		status := "active"
		if !key.RevokedAt.IsZero() {
			status = "revoked"
		}
		keyUsage := usage[key.Name]
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", key.Name, status,
			formatTokenCount(keyUsage.InputTokens), formatTokenCount(keyUsage.InputDetails.CachedTokens),
			formatTokenCount(keyUsage.OutputTokens), formatTokenCount(keyUsage.TotalTokens), key.CreatedAt.Format(time.RFC3339))
	}
	return w.Flush()
}

func importLegacyAPIKey(store *StateStore, secret string) error {
	keys, err := store.readAPIKeys()
	if err != nil {
		return err
	}
	if len(keys) != 0 || secret == "" {
		return nil
	}
	return store.addAPIKey(storedAPIKey{Name: "legacy", Secret: secret, CreatedAt: time.Now()})
}
