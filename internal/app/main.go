package app

import (
	"errors"
	"fmt"
	"os"
	"runtime/debug"
)

const usage = `codex-balancer spreads Codex turns across several ChatGPT accounts.

Usage:
  codex-balancer <command> [flags]

Commands:
  server     Serve the balancing proxy
  accounts   Manage the account pool
  keys       Manage client API keys
  version    Print the version

Run "codex-balancer <command> -h" for flags.
`

// Run executes the command-line application with args.
func Run(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("no command given")
	}
	switch args[0] {
	case "server":
		return serverCmd(args[1:])
	case "accounts":
		return accountsCmd(args[1:])
	case "keys":
		return keysCmd(args[1:])
	case "version":
		fmt.Println(version())
		return nil
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	}
	fmt.Fprint(os.Stderr, usage)
	return fmt.Errorf("unknown command %q", args[0])
}

func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			return s.Value
		}
	}
	return info.Main.Version
}
