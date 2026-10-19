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

	"github.com/zacharyhill/gosling/internal/transpile"
	"golang.org/x/mod/modfile"
)

const sourceExt = ".gol"

var errNoSource = errors.New("no " + sourceExt + " files")

type sourceFile struct {
	path        string
	packageName string
	imports     []string
	// context is the build configuration that type-checks a .gol file.
	context          build.Context
	isInDefaultBuild bool
}

type packageDir struct {
	dir        string
	importPath string
	golFiles   []sourceFile
	goFiles    []sourceFile // hand-written .go files
	// orphans holds generated .go files whose .gol file no longer exists,
	// for example after a rename of the .gol file.
	orphans []string
	// sources holds file contents that replace the files on disk, such as
	// unsaved editor text.
	sources map[string][]byte
}

// expandDirs changes command-line patterns into directories. A pattern that
// ends in /... adds each directory below it that contains .gol files or
// generated .go files. A generated file without its .gol file is an orphan
// that gosling removes.
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
		if err := walkGoslingDirs(root, seen, add); err != nil {
			return nil, err
		}
	}
	return dirs, nil
}

// walkGoslingDirs calls add for each directory below root that has gosling
// files. It skips testdata, vendor, and directories that start with . or _.
func walkGoslingDirs(root string, seen map[string]bool, add func(string)) error {
	return filepath.WalkDir(root, func(entryPath string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			if seen[filepath.Dir(entryPath)] {
				return nil
			}
			isGosling, err := isGoslingFile(entryPath)
			if isGosling {
				add(filepath.Dir(entryPath))
			}
			return err
		}
		if entryPath != root && isSkippedDir(entry.Name()) {
			return filepath.SkipDir
		}
		return nil
	})
}

func isSkippedDir(name string) bool {
	return name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

// isGoslingFile reports whether a file makes its directory a gosling
// directory.
func isGoslingFile(filePath string) (bool, error) {
	if strings.HasSuffix(filePath, sourceExt) {
		return true, nil
	}
	if !strings.HasSuffix(filePath, ".go") {
		return false, nil
	}
	return transpile.IsGeneratedFile(filePath)
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
	if len(dirInfo.golFiles) == 0 && len(dirInfo.orphans) == 0 {
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

// addFile records a .gol file or a hand-written .go file, with its build
// configuration. It skips generated .go files, and records the orphans.
func (packageDir *packageDir) addFile(name string, generated map[string]bool) error {
	filePath := filepath.Join(packageDir.dir, name)
	isGol := strings.HasSuffix(name, sourceExt)
	if !isGol && (!strings.HasSuffix(name, ".go") || generated[name]) {
		return nil
	}
	src, err := packageDir.readSource(filePath)
	if err != nil {
		return err
	}
	if !isGol && transpile.IsGenerated(src) {
		packageDir.orphans = append(packageDir.orphans, filePath)
		return nil
	}
	source, err := readHeader(filePath, src)
	if err != nil {
		return err
	}
	if !isGol {
		source.isInDefaultBuild, err = build.Default.MatchFile(packageDir.dir, name)
		packageDir.goFiles = append(packageDir.goFiles, source)
		return err
	}
	if source.context, err = chooseContext(filePath, src); err != nil {
		return err
	}
	source.isInDefaultBuild = isDefaultContext(source.context)
	packageDir.golFiles = append(packageDir.golFiles, source)
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
	absoluteDir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for searchDir := absoluteDir; ; searchDir = filepath.Dir(searchDir) {
		goModPath := filepath.Join(searchDir, "go.mod")
		goModContent, err := os.ReadFile(goModPath)
		if err == nil {
			modulePath := modfile.ModulePath(goModContent)
			if modulePath == "" {
				return "", fmt.Errorf("%s: no module path", goModPath)
			}
			relativePath, err := filepath.Rel(searchDir, absoluteDir)
			if err != nil {
				return "", err
			}
			return path.Join(modulePath, filepath.ToSlash(relativePath)), nil
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
	dirsByImportPath := map[string]*packageDir{}
	for _, dirInfo := range dirs {
		dirsByImportPath[dirInfo.importPath] = dirInfo
	}
	var sorted []*packageDir
	visited := map[*packageDir]bool{}
	var visit func(dirInfo *packageDir)
	visit = func(dirInfo *packageDir) {
		if visited[dirInfo] {
			return
		}
		visited[dirInfo] = true
		for _, source := range concatFiles(dirInfo.golFiles, dirInfo.goFiles) {
			for _, importedPath := range source.imports {
				if dependency, ok := dirsByImportPath[importedPath]; ok {
					visit(dependency)
				}
			}
		}
		sorted = append(sorted, dirInfo)
	}
	for _, dirInfo := range dirs {
		visit(dirInfo)
	}
	return sorted
}
