// Command gosling converts .gol files to .go files in the same directory.
// A .gol file is Go with some TypeScript syntax, such as try/catch.
//
// Usage:
//
//	gosling [-check] [-lines=false] [-v] [dir | dir/...]...
//	gosling lsp [-gopls path]
//
// With no arguments, gosling converts the current directory. To use go
// generate, add "//go:generate gosling" to a .go file. gosling lsp runs a
// language server for editors.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/zacharyhill/gosling/internal/transpile"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "lsp" {
		return runLSP(args[1:], stderr)
	}
	flags := flag.NewFlagSet("gosling", flag.ContinueOnError)
	flags.SetOutput(stderr)
	check := flags.Bool("check", false, "report out-of-date outputs and exit 1 instead of writing")
	lines := flags.Bool("lines", true, "emit line directives so errors and stack traces point at .gol files")
	verbose := flags.Bool("v", false, "print progress while generating (-check always does)")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "usage: gosling [-check] [-lines=false] [-v] [dir | dir/...]...")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return 2
	}
	patterns := flags.Args()
	implicit := len(patterns) == 0
	if implicit {
		patterns = []string{"."}
	}
	options := generateOptions{check: *check, lineDirectives: *lines, verbose: *verbose || *check, stderr: stderr}
	if err := generate(patterns, options); err != nil {
		fmt.Fprintln(stderr, err)
		if implicit && errors.Is(err, errNoSource) {
			fmt.Fprintln(stderr, `run "gosling ./..." to include subdirectories`)
		}
		return 1
	}
	return 0
}

var errStale = errors.New("generated files are out of date; run gosling")

type generateOptions struct {
	check          bool
	lineDirectives bool
	verbose        bool
	stderr         io.Writer
}

func generate(patterns []string, options generateOptions) error {
	dirs, err := loadDirs(patterns)
	if err != nil {
		return err
	}
	outputs, err := transpileDirs(dirs, options)
	if err != nil {
		return err
	}
	return writeOutputs(outputs, options)
}

func loadDirs(patterns []string) ([]*packageDir, error) {
	dirPaths, err := expandDirs(patterns)
	if err != nil {
		return nil, err
	}
	dirs := make([]*packageDir, 0, len(dirPaths))
	for _, dirPath := range dirPaths {
		dirInfo, err := loadDir(dirPath, nil)
		if err != nil {
			return nil, err
		}
		dirs = append(dirs, dirInfo)
	}
	return sortByImports(dirs), nil
}

// transpileDirs converts dirs in dependency order. Each output goes into the
// overlay when it is produced. Packages that import it then type-check
// against the new code, also in -check mode.
func transpileDirs(dirs []*packageDir, options generateOptions) ([]transpile.Output, error) {
	action := "generating"
	if options.check {
		action = "checking"
	}
	overlay := map[string][]byte{}
	var outputs []transpile.Output
	for dirIndex, dirInfo := range dirs {
		if options.verbose {
			fmt.Fprintf(options.stderr, "%s %s (%d/%d)\n", action, dirInfo.dir, dirIndex+1, len(dirs))
		}
		dirOutputs, err := transpileDir(dirInfo, overlay, options.lineDirectives)
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, dirOutputs...)
	}
	return outputs, nil
}

func transpileDir(dirInfo *packageDir, overlay map[string][]byte, lineDirectives bool) ([]transpile.Output, error) {
	groups, err := dirInfo.groups()
	if err != nil {
		return nil, err
	}
	var outputs []transpile.Output
	for _, fileGroup := range groups {
		groupOutputs, err := transpileGroup(dirInfo, fileGroup, overlay, transpileOptions{lineDirectives: lineDirectives})
		if err != nil {
			return nil, err
		}
		for _, output := range groupOutputs {
			absPath, err := filepath.Abs(output.Path)
			if err != nil {
				return nil, err
			}
			overlay[absPath] = output.Src
		}
		outputs = append(outputs, groupOutputs...)
	}
	return outputs, nil
}

// writeOutputs writes changed outputs, or in -check mode reports them.
func writeOutputs(outputs []transpile.Output, options generateOptions) error {
	stale := false
	written := 0
	for _, output := range outputs {
		changed, err := needsWrite(output)
		if err != nil {
			return err
		}
		if !changed {
			continue
		}
		if options.check {
			fmt.Fprintf(options.stderr, "%s is out of date\n", output.Path)
			stale = true
			continue
		}
		if err := os.WriteFile(output.Path, output.Src, 0o644); err != nil {
			return err
		}
		written++
	}
	if stale {
		return errStale
	}
	switch {
	case options.check:
		fmt.Fprintf(options.stderr, "%d generated files up to date\n", len(outputs))
	case options.verbose:
		fmt.Fprintf(options.stderr, "wrote %d generated files, %d unchanged\n", written, len(outputs)-written)
	}
	return nil
}

// needsWrite reports whether output differs from the file on disk. It returns
// an error for a .go file that gosling did not generate.
func needsWrite(output transpile.Output) (bool, error) {
	existing, err := os.ReadFile(output.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if !bytes.HasPrefix(existing, []byte(transpile.GeneratedPrefix)) {
		return false, fmt.Errorf("%s exists and was not generated by gosling; rename it or the .gol file", output.Path)
	}
	return !bytes.Equal(existing, output.Src), nil
}
