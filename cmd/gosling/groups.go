package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/zacharyhill/gosling/internal/transpile"
)

// group is a set of files that gosling type-checks together.
type group struct {
	inputs  []sourceFile
	emit    []sourceFile // subset of inputs whose outputs this group produces
	goFiles []sourceFile
}

func isTestFile(source sourceFile) bool {
	name := strings.TrimSuffix(strings.TrimSuffix(source.path, sourceExt), ".go")
	return strings.HasSuffix(name, "_test")
}

type filesByRole struct {
	base, internal, external []sourceFile
}

func (roles *filesByRole) add(source sourceFile, pkg string) error {
	switch {
	case !isTestFile(source):
		roles.base = append(roles.base, source)
	case source.packageName == pkg:
		roles.internal = append(roles.internal, source)
	case source.packageName == pkg+"_test":
		roles.external = append(roles.external, source)
	default:
		return fmt.Errorf("%s: unexpected package %s, want %s or %s_test", source.path, source.packageName, pkg, pkg)
	}
	return nil
}

// groups splits a directory into the package, its internal tests, and its
// external test package. Internal tests share a package scope with the
// package, so they are type-checked with its .gol files.
func (packageDir *packageDir) groups() ([]group, error) {
	pkg, err := packageDir.packageName()
	if err != nil {
		return nil, err
	}
	var golRoles, goRoles filesByRole
	for _, source := range packageDir.golFiles {
		if err := golRoles.add(source, pkg); err != nil {
			return nil, err
		}
	}
	for _, source := range packageDir.goFiles {
		if err := goRoles.add(source, pkg); err != nil {
			return nil, err
		}
	}
	var groups []group
	if len(golRoles.base) > 0 {
		groups = append(groups, group{inputs: golRoles.base, emit: golRoles.base, goFiles: goRoles.base})
	}
	if len(golRoles.internal) > 0 {
		groups = append(groups, group{
			inputs:  concatFiles(golRoles.base, golRoles.internal),
			emit:    golRoles.internal,
			goFiles: concatFiles(goRoles.base, goRoles.internal),
		})
	}
	if len(golRoles.external) > 0 {
		groups = append(groups, group{inputs: golRoles.external, emit: golRoles.external, goFiles: goRoles.external})
	}
	return groups, nil
}

// packageName returns the package the directory's non-test files declare.
func (packageDir *packageDir) packageName() (string, error) {
	allFiles := concatFiles(packageDir.golFiles, packageDir.goFiles)
	pkg := ""
	for _, source := range allFiles {
		if isTestFile(source) {
			continue
		}
		if pkg != "" && source.packageName != pkg {
			return "", fmt.Errorf("%s: found packages %s and %s", packageDir.dir, pkg, source.packageName)
		}
		pkg = source.packageName
	}
	if pkg == "" {
		// The directory has only test files. Remove _test to get the package name.
		pkg = strings.TrimSuffix(allFiles[0].packageName, "_test")
	}
	return pkg, nil
}

func concatFiles(first, second []sourceFile) []sourceFile {
	return append(append([]sourceFile{}, first...), second...)
}

func transpileGroup(dir string, fileGroup group, overlay map[string][]byte, lines bool) ([]transpile.Output, error) {
	importer, err := newImporter(dir, concatFiles(fileGroup.inputs, fileGroup.goFiles), overlay)
	if err != nil {
		return nil, err
	}
	inputs := make([]transpile.Input, 0, len(fileGroup.inputs))
	for _, source := range fileGroup.inputs {
		src, err := os.ReadFile(source.path)
		if err != nil {
			return nil, err
		}
		inputs = append(inputs, transpile.Input{Path: source.path, Src: src})
	}
	goFilePaths := make([]string, 0, len(fileGroup.goFiles))
	for _, source := range fileGroup.goFiles {
		goFilePaths = append(goFilePaths, source.path)
	}
	outputs, err := transpile.Transpile(transpile.Config{Importer: importer, GoFiles: goFilePaths, LineDirectives: lines}, inputs)
	if err != nil {
		return nil, err
	}
	wantedPaths := map[string]bool{}
	for _, source := range fileGroup.emit {
		wantedPaths[transpile.OutputPath(source.path)] = true
	}
	kept := outputs[:0]
	for _, output := range outputs {
		if wantedPaths[output.Path] {
			kept = append(kept, output)
		}
	}
	return kept, nil
}
