package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/scanner"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/zacharyhill/gosling/internal/golfmt"
)

type fmtOptions struct {
	shouldList, shouldWrite bool
	stdout                  io.Writer
}

// runFmt formats .gol files. Without -l or -w, it prints each result.
func runFmt(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("gosling fmt", flag.ContinueOnError)
	flags.SetOutput(stderr)
	list := flags.Bool("l", false, "list files whose formatting differs")
	write := flags.Bool("w", false, "write the result to the file instead of printing it")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "usage: gosling fmt [-l] [-w] [file | dir | dir/...]...")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return 2
	}
	patterns := flags.Args()
	if len(patterns) == 0 {
		patterns = []string{"."}
	}
	paths, err := findGolPaths(patterns)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	options := fmtOptions{shouldList: *list, shouldWrite: *write, stdout: stdout}
	exitCode := 0
	for _, path := range paths {
		if err := formatFile(path, options); err != nil {
			fmt.Fprintln(stderr, err)
			exitCode = 1
		}
	}
	return exitCode
}

// findGolPaths returns the .gol files of the patterns. A directory gives its
// own .gol files. A pattern that ends in /... also gives the files below it,
// with the directories that gosling ./... skips.
func findGolPaths(patterns []string) ([]string, error) {
	var paths []string
	for _, pattern := range patterns {
		root, recursive := strings.CutSuffix(pattern, "...")
		if recursive {
			found, err := findGolFilesBelow(filepath.Clean(strings.TrimSuffix(root, "/")))
			if err != nil {
				return nil, err
			}
			paths = append(paths, found...)
			continue
		}
		info, err := os.Stat(pattern)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			paths = append(paths, pattern)
			continue
		}
		found, err := filepath.Glob(filepath.Join(pattern, "*"+sourceExt))
		if err != nil {
			return nil, err
		}
		paths = append(paths, found...)
	}
	return paths, nil
}

func findGolFilesBelow(root string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(entryPath string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && entryPath != root && isSkippedDir(entry.Name()) {
			return filepath.SkipDir
		}
		if !entry.IsDir() && strings.HasSuffix(entryPath, sourceExt) {
			paths = append(paths, entryPath)
		}
		return nil
	})
	return paths, err
}

func formatFile(path string, options fmtOptions) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	formatted, err := golfmt.Source(src)
	if err != nil {
		return addPathToError(path, err)
	}
	hasChanged := !bytes.Equal(src, formatted)
	if options.shouldList && hasChanged {
		fmt.Fprintln(options.stdout, path)
	}
	if options.shouldWrite && hasChanged {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		return os.WriteFile(path, formatted, info.Mode().Perm())
	}
	if !options.shouldList && !options.shouldWrite {
		_, err := options.stdout.Write(formatted)
		return err
	}
	return nil
}

// addPathToError adds path to a format error. A syntax error already starts with
// its line and column.
func addPathToError(path string, err error) error {
	var positioned *scanner.Error
	var positionedList scanner.ErrorList
	if errors.As(err, &positioned) || errors.As(err, &positionedList) {
		return fmt.Errorf("%s:%w", path, err)
	}
	return fmt.Errorf("%s: %w", path, err)
}
