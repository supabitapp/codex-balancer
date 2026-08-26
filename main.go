package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/supabitapp/codex-balancer/internal/app"
)

func main() {
	if err := app.Run(os.Args[1:]); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(os.Stderr, "codex-balancer: %v\n", err)
		}
		os.Exit(1)
	}
}
