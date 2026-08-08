package main

import (
	"fmt"
	"os"

	"github.com/nexusriot/s3duck-tui/pkg/controller"
)

func main() {
	if err := controller.NewController().Run(); err != nil {
		// Run returns only once the tview loop has stopped, so the terminal is
		// ours again and writing to stderr can't corrupt the display.
		fmt.Fprintln(os.Stderr, "s3duck-tui:", err)
		os.Exit(1)
	}
}
