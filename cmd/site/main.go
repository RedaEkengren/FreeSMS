// Command site writes the marketing page from site/src, through the
// product's own catalogue and code. Run from the repository root:
//
//	go run ./cmd/site
package main

import (
	"fmt"
	"os"

	"github.com/RedaEkengren/FreeSMS/internal/site"
)

func main() {
	page, err := site.Generate(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(site.Output, page, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
