package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"

	"github.com/zacharyhill/gosling/internal/lsp"
	"github.com/zacharyhill/gosling/internal/transpile"
)

// runLSP runs the language server on stdin and stdout.
func runLSP(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("gosling lsp", flag.ContinueOnError)
	flags.SetOutput(stderr)
	goplsPath := flags.String("gopls", "gopls", "gopls command to run")
	generateOnSave := flags.Bool("generate-on-save", false, "write the generated .go files when the editor saves a .gol file")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "usage: gosling lsp [-gopls path] [-generate-on-save]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	options := lsp.Options{
		Generator:      generator{},
		GoplsPath:      *goplsPath,
		GenerateOnSave: *generateOnSave,
		Logger:         log.New(stderr, "gosling lsp: ", log.LstdFlags),
	}
	if err := lsp.Serve(ctx, os.Stdin, os.Stdout, options); err != nil {
		fmt.Fprintln(stderr, "gosling lsp:", err)
		return 1
	}
	return 0
}

// generator transpiles one directory for the language server.
type generator struct{}

// Write generates dir from the files on disk with line directives, as the
// gosling command does by default.
func (generator) Write(dir string) error {
	return generate([]string{dir}, generateOptions{lineDirectives: true, stderr: io.Discard})
}

// Generate converts each group of the directory. It keeps going after
// errors, so it can return outputs together with the joined errors.
func (generator) Generate(dir string, sources map[string][]byte, lineDirectives bool) ([]transpile.Output, error) {
	dirInfo, err := loadDir(dir, sources)
	// A directory with only orphans has nothing to generate.
	if err != nil || len(dirInfo.golFiles) == 0 {
		return nil, err
	}
	groups, err := dirInfo.groups()
	if err != nil {
		return nil, err
	}
	var outputs []transpile.Output
	var groupErrors []error
	options := transpileOptions{lineDirectives: lineDirectives, keepGoing: true}
	for _, fileGroup := range groups {
		groupOutputs, err := transpileGroup(dirInfo, fileGroup, map[string][]byte{}, options)
		outputs = append(outputs, groupOutputs...)
		if err != nil {
			groupErrors = append(groupErrors, err)
		}
	}
	return outputs, errors.Join(groupErrors...)
}
