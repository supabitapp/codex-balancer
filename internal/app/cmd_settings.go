package app

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
)

const settingsHelp = `Manage global settings.

Usage:
  codex-balancer settings set fast-mode <default|on|off>
  codex-balancer settings get fast-mode
  codex-balancer settings list [-json]

Fast mode: default respects the client; on forces fast; off forces standard.
Changes restart existing WebSockets when a running server applies them.

Flags (place before the setting name):
  -state string  state database (default %s)
  -json          machine-readable output, get/list only
`

func settingsCmd(args []string) error {
	help := func() { fmt.Fprintf(os.Stdout, settingsHelp, defaultStatePath()) }
	if len(args) == 0 {
		help()
		return errors.New("no subcommand given")
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		help()
		return nil
	}
	fs := flag.NewFlagSet("settings", flag.ContinueOnError)
	fs.Usage = help
	path := fs.String("state", defaultStatePath(), "state database")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	switch args[0] {
	case "set":
		if fs.NArg() != 2 || fs.Arg(0) != "fast-mode" || !fastMode(fs.Arg(1)).valid() || *asJSON {
			return errors.New("usage: settings set [-state path] fast-mode <default|on|off>")
		}
	case "get":
		if fs.NArg() != 1 || fs.Arg(0) != "fast-mode" {
			return errors.New("usage: settings get [-json] [-state path] fast-mode")
		}
	case "list":
		if fs.NArg() != 0 {
			return errors.New("usage: settings list [-json] [-state path]")
		}
	default:
		return fmt.Errorf("unknown settings subcommand %q", args[0])
	}
	store, err := openStateStore(*path)
	if err != nil {
		return err
	}
	defer store.Close()
	if args[0] == "set" {
		if err := store.raw.SetFastMode(fs.Arg(1)); err != nil {
			return err
		}
		fmt.Printf("Saved fast-mode=%s. Running servers apply changes on their next settings poll (500 ms).\n", fs.Arg(1))
		return nil
	}
	mode, err := store.raw.FastMode()
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"fast-mode": mode})
	}
	if args[0] == "get" {
		fmt.Println(mode)
	} else {
		fmt.Printf("fast-mode\t%s\n", mode)
	}
	return nil
}
