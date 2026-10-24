// Command driftlens reports semantic drift between Git desired state and live
// Kubernetes objects.
package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/archit2501/driftlens/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	isTTY := false
	if st, err := os.Stdout.Stat(); err == nil {
		isTTY = st.Mode()&os.ModeCharDevice != 0
	}
	code := cli.Run(ctx, os.Args[1:], cli.IO{In: os.Stdin, Out: os.Stdout, Err: os.Stderr, IsTerminal: isTTY})
	stop()
	os.Exit(code)
}
