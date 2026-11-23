// labctl is the command-line client for the clusterd platform API.
package main

import (
	"os"

	"github.com/jurrebuunk/clusterd/internal/labctl"
)

func main() {
	os.Exit(labctl.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
