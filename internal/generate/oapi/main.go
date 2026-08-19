// Command oapi validates api/openapi.yaml so the public contract fails the
// build before it can drift from the handlers and the web client.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/getkin/kin-openapi/openapi3"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "gen" {
		fmt.Fprintln(os.Stderr, "usage: oapi gen")
		os.Exit(2)
	}
	path := "api/openapi.yaml"
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "oapi:", err)
		os.Exit(1)
	}
	if err := doc.Validate(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "oapi:", err)
		os.Exit(1)
	}
	fmt.Printf("oapi: %s valid (%d paths, %d schemas)\n", path, len(doc.Paths.Map()), len(doc.Components.Schemas))
}
