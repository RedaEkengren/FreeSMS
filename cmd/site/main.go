// Command site writes the marketing pages from site/src, one per language, through the
// product's own catalogue and code. Run from the repository root:
//
//	go run ./cmd/site
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/RedaEkengren/FreeSMS/internal/site"
)

func main() {
	for _, p := range site.Pages {
		page, err := site.Generate(".", p)
		if err != nil {
			fmt.Fprintln(os.Stderr, p.Source+":", err)
			os.Exit(1)
		}
		if err := os.MkdirAll(filepath.Dir(p.Output), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := os.WriteFile(p.Output, page, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}
