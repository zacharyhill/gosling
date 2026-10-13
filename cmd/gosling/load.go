package main

import (
	"errors"
	"fmt"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/mod/modfile"
)

const sourceExt = ".gol"

var errNoSource = errors.New("no " + sourceExt + " files")

type sourceFile struct {
	path        string
	packageName string
	imports     []string
}

type packageDir struct {
	dir        string
	importPath string
	golFiles   []sourceFile
	goFiles    []sourceFile // hand-written .go files that match the build context
	// sources holds file contents that replace the files on disk, such as
	// unsaved editor text.
	sources map[string][]byte
}

// expandDirs changes command-line patterns into directories. A pattern that
// ends in /... adds each directory below it that contains .gol files.
func expandDirs(patterns []string) ([]string, error) {
	var dirs []string
	seen := map[string]bool{}
	add := func(dir string) {
		dir = filepath.Clean(dir)
		if !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}
	for _, pattern := range patterns {
		root, recursive := strings.CutSuffix(pattern, "...")
		if !recursive {
			add(pattern)
			continue
		}
		root = filepath.Clean(strings.TrimSuffix(root, "/"))
		err := filepath.WalkDir(root, func(entryPath string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() {
				if strings.HasSuffix(entryPath, sourceExt) {
					add(filepath.Dir(entryPath))
				}
				return nil
			}
			name := entry.Name()
			if entryPath != root && (name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return dirs, nil
}

func loadDir(dir string, sources map[string][]byte) (*packageDir, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	dirInfo := &packageDir{dir: dir, sources: sources}
	generated := generatedNames(entries)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if err := dirInfo.addFile(entry.Name(), generated); err != nil {
			return nil, err
		}
	}
	if len(dirInfo.golFiles) == 0 {
		return nil, fmt.Errorf("%s: %w", dir, errNoSource)
	}
	dirInfo.importPath, err = importPath(dir)
	if err != nil {
		return nil, err
	}
	return dirInfo, nil
}

// generatedNames returns the .go file names that .gol files in the
// directory generate.
func generatedNames(entries []fs.DirEntry) map[string]bool {
	generated := map[string]bool{}
	for _, entry := range entries {
		if name, ok := strings.CutSuffix(entry.Name(), sourceExt); ok && !entry.IsDir() {
			generated[name+".go"] = true
		}
	}
	return generated
}

// addFile records a .gol file, or a hand-written .go file that matches the
// build context. It skips generated .go files.
func (packageDir *packageDir) addFile(name string, generated map[string]bool) error {
	filePath := filepath.Join(packageDir.dir, name)
	isGol := strings.HasSuffix(name, sourceExt)
	if !isGol {
		if !strings.HasSuffix(name, ".go") || generated[name] {
			return nil
		}
		matches, err := build.Default.MatchFile(packageDir.dir, name)
		if err != nil || !matches {
			return err
		}
	}
	src, err := packageDir.readSource(filePath)
	if err != nil {
		return err
	}
	source, err := readHeader(filePath, src)
	if err != nil {
		return err
	}
	if isGol {
		packageDir.golFiles = append(packageDir.golFiles, source)
	} else {
		packageDir.goFiles = append(packageDir.goFiles, source)
	}
	return nil
}

func (packageDir *packageDir) readSource(filePath string) ([]byte, error) {
	if src, ok := packageDir.sources[filePath]; ok {
		return src, nil
	}
	return os.ReadFile(filePath)
}

// readHeader parses the package clause and imports. Gosling syntax occurs only
// after the imports, so the Go parser can read this part of a .gol file.
func readHeader(filePath string, src []byte) (sourceFile, error) {
	astFile, err := parser.ParseFile(token.NewFileSet(), filePath, src, parser.ImportsOnly)
	if err != nil {
		return sourceFile{}, err
	}
	source := sourceFile{path: filePath, packageName: astFile.Name.Name}
	for _, importSpec := range astFile.Imports {
		importedPath, err := strconv.Unquote(importSpec.Path.Value)
		if err != nil {
			return sourceFile{}, err
		}
		source.imports = append(source.imports, importedPath)
	}
	return source, nil
}

func importPath(dir string) (string, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for searchDir := absDir; ; searchDir = filepath.Dir(searchDir) {
		goModBytes, err := os.ReadFile(filepath.Join(searchDir, "go.mod"))
		if err == nil {
			modulePath := modfile.ModulePath(goModBytes)
			if modulePath == "" {
				return "", fmt.Errorf("%s: no module path", filepath.Join(searchDir, "go.mod"))
			}
			relPath, err := filepath.Rel(searchDir, absDir)
			if err != nil {
				return "", err
			}
			return path.Join(modulePath, filepath.ToSlash(relPath)), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		if filepath.Dir(searchDir) == searchDir {
			return "", fmt.Errorf("%s: not inside a Go module", dir)
		}
	}
}

func sortByImports(dirs []*packageDir) []*packageDir {
	byPath := map[string]*packageDir{}
	for _, dirInfo := range dirs {
		byPath[dirInfo.importPath] = dirInfo
	}
	var out []*packageDir
	visited := map[*packageDir]bool{}
	var visit func(dirInfo *packageDir)
	visit = func(dirInfo *packageDir) {
		if visited[dirInfo] {
			return
		}
		visited[dirInfo] = true
		for _, source := range append(dirInfo.golFiles, dirInfo.goFiles...) {
			for _, importedPath := range source.imports {
				if dependency, ok := byPath[importedPath]; ok {
					visit(dependency)
				}
			}
		}
		out = append(out, dirInfo)
	}
	for _, dirInfo := range dirs {
		visit(dirInfo)
	}
	return out
}
