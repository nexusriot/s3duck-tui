package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/nexusriot/s3duck-tui/internal/cli"
	"github.com/nexusriot/s3duck-tui/internal/controller"
	"github.com/nexusriot/s3duck-tui/internal/ui/widgets"
)

func main() {
	os.Exit(run(os.Args))
}

// run is main with an exit status, so every early return still unwinds
// normally. 2 is a command-line mistake, 1 a failure while running.
func run(args []string) int {
	prog := filepath.Base(args[0])
	opts, err := cli.Parse(args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n\n%s", prog, err, cli.Usage(prog))
		return 2
	}
	if opts.Help {
		fmt.Print(cli.Usage(prog))
		return 0
	}
	if opts.Version {
		fmt.Println(widgets.VersionText())
		return 0
	}
	if opts.ConfigPath != "" {
		// A missing default config is created on first run; a missing
		// --config is almost always a typo, and creating it would open an
		// empty profile list with no hint why.
		if _, err := os.Stat(opts.ConfigPath); err != nil {
			fmt.Fprintf(os.Stderr, "%s: --config: %v\n", prog, err)
			return 2
		}
	}
	if opts.Notify == "" {
		mode, err := cli.ParseNotifyMode(os.Getenv("S3DUCK_NOTIFY"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: S3DUCK_NOTIFY: %v\n", prog, err)
			return 2
		}
		opts.Notify = mode
	}

	c := controller.NewController(opts)
	if err := c.ValidateLaunch(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", prog, err)
		return 2
	}
	if err := c.Run(); err != nil {
		// Run returns only once the tview loop has stopped, so the terminal is
		// ours again and writing to stderr can't corrupt the display.
		fmt.Fprintln(os.Stderr, prog+":", err)
		return 1
	}
	return 0
}
