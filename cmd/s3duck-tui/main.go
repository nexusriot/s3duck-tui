package main

import (
	"github.com/nexusriot/s3duck-tui/pkg/controller"
)

func main() {
	controller.NewController(false).Run()
}
