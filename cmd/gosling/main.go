// Command gosling converts .gol files to .go files in the same directory.
// A .gol file is Go with some TypeScript syntax, such as try/catch.
//
// Usage:
//
//	gosling [-check] [-lines=false] [-v] [dir | dir/...]...
//	gosling -version
//	gosling fmt [-l] [-w] [file | dir | dir/...]...
//	gosling lint command [arg...]
//	gosling lsp [-gopls path] [-generate-on-save]
//
// With no arguments, gosling converts the current directory. To use go
// generate, add "//go:generate gosling" to a .go file. gosling fmt formats
// .gol files. gosling lint runs a linter and shows its positions in .gol
// files. gosling lsp runs a language server for editors.
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
	"runtime/debug"

	"github.com/zacharyhill/gosling/internal/transpile"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "lsp" {
		return runLSP(args[1:], stderr)
	}
	if len(args) > 0 && args[0] == "fmt" {
		return runFmt(args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "lint" {
		return runLint(args[1:], stdout, stderr)
	}
	flags := flag.NewFlagSet("gosling", flag.ContinueOnError)
	flags.SetOutput(stderr)
	check := flags.Bool("check", false, "report out-of-date outputs and exit 1 instead of writing")
	lines := flags.Bool("lines", true, "emit line directives so errors and stack traces point at .gol files")
	verbose := flags.Bool("v", false, "print progress while generating (-check always does)")
	showVersion := flags.Bool("version", false, "print the gosling version and exit")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "usage: gosling [-check] [-lines=false] [-v] [-version] [dir | dir/...]...")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Fprintln(stdout, "gosling", version())
		return 0
	}
	patterns := flags.Args()
	isDefaultDir := len(patterns) == 0
	if isDefaultDir {
		patterns = []string{"."}
	}
	options := generateOptions{check: *check, lineDirectives: *lines, verbose: *verbose || *check, stderr: stderr}
	if err := generate(patterns, options); err != nil {
		fmt.Fprintln(stderr, err)
		if isDefaultDir && errors.Is(err, errNoSource) {
			fmt.Fprintln(stderr, `run "gosling ./..." to include subdirectories`)
		}
		return 1
	}
	return 0
}

// version returns the module version that go install or go build stamps
// into the binary.
func version() string {
	buildInfo, ok := debug.ReadBuildInfo()
	if !ok {
		return "(unknown)"
	}
	return buildInfo.Main.Version
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
	foundStale, err := removeOrphans(dirs, options)
	if err != nil {
		return err
	}
	outputs, err := transpileDirs(withSources(dirs), options)
	if err != nil {
		return err
	}
	return writeOutputs(outputs, options, foundStale)
}

// withSources returns the directories that have .gol files. A directory can
// hold only orphans.
func withSources(dirs []*packageDir) []*packageDir {
	var kept []*packageDir
	for _, dirInfo := range dirs {
		if len(dirInfo.golFiles) > 0 {
			kept = append(kept, dirInfo)
		}
	}
	return kept
}

// removeOrphans deletes generated files whose .gol file no longer exists. In
// -check mode, it reports them instead. It returns true if it reports one.
func removeOrphans(dirs []*packageDir, options generateOptions) (bool, error) {
	foundStale := false
	for _, dirInfo := range dirs {
		for _, orphan := range dirInfo.orphans {
			if options.check {
				fmt.Fprintf(options.stderr, "%s has no source file; run gosling to remove it\n", orphan)
				foundStale = true
				continue
			}
			if err := os.Remove(orphan); err != nil {
				return false, err
			}
			fmt.Fprintf(options.stderr, "removed %s: its %s file no longer exists\n", orphan, sourceExt)
		}
	}
	return foundStale, nil
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
// overlay at once. Packages that import it then type-check against the new
// code, also in -check mode.
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
			absolutePath, err := filepath.Abs(output.Path)
			if err != nil {
				return nil, err
			}
			overlay[absolutePath] = output.Src
		}
		outputs = append(outputs, groupOutputs...)
	}
	return outputs, nil
}

// writeOutputs writes changed outputs, or in -check mode reports them.
// foundStale is true if an earlier step found out-of-date files.
func writeOutputs(outputs []transpile.Output, options generateOptions, foundStale bool) error {
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
			foundStale = true
			continue
		}
		if err := os.WriteFile(output.Path, output.Src, 0o644); err != nil {
			return err
		}
		written++
	}
	if foundStale {
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
	if !transpile.IsGenerated(existing) {
		return false, fmt.Errorf("%s exists and was not generated by gosling; rename it or the .gol file", output.Path)
	}
	return !bytes.Equal(existing, output.Src), nil
}
