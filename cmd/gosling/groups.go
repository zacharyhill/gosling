package main

import (
	"fmt"
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

func (filesByRole *filesByRole) add(source sourceFile, packageName string) error {
	switch {
	case !isTestFile(source):
		filesByRole.base = append(filesByRole.base, source)
	case source.packageName == packageName:
		filesByRole.internal = append(filesByRole.internal, source)
	case source.packageName == packageName+"_test":
		filesByRole.external = append(filesByRole.external, source)
	default:
		return fmt.Errorf("%s: unexpected package %s, want %s or %s_test", source.path, source.packageName, packageName, packageName)
	}
	return nil
}

// groups splits a directory into the package, its internal tests, and its
// external test package. Internal tests share a package scope with the
// package, so gosling type-checks them with its .gol files.
func (packageDir *packageDir) groups() ([]group, error) {
	packageName, err := packageDir.packageName()
	if err != nil {
		return nil, err
	}
	var golRoles, goRoles filesByRole
	for _, source := range packageDir.golFiles {
		if err := golRoles.add(source, packageName); err != nil {
			return nil, err
		}
	}
	for _, source := range packageDir.goFiles {
		if err := goRoles.add(source, packageName); err != nil {
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
	name := ""
	for _, source := range allFiles {
		if isTestFile(source) {
			continue
		}
		if name != "" && source.packageName != name {
			return "", fmt.Errorf("%s: found packages %s and %s", packageDir.dir, name, source.packageName)
		}
		name = source.packageName
	}
	if name == "" {
		// The directory has only test files. Remove _test to get the package name.
		name = strings.TrimSuffix(allFiles[0].packageName, "_test")
	}
	return name, nil
}

func concatFiles(first, second []sourceFile) []sourceFile {
	return append(append([]sourceFile{}, first...), second...)
}

// transpileOptions holds the transpile.Config settings that callers choose.
type transpileOptions struct {
	lineDirectives bool
	keepGoing      bool
}

// transpileGroup converts the files of one group. With keepGoing, it can
// return outputs together with an error.
func transpileGroup(dirInfo *packageDir, fileGroup group, overlay map[string][]byte, options transpileOptions) ([]transpile.Output, error) {
	importer, err := newImporter(dirInfo.dir, concatFiles(fileGroup.inputs, fileGroup.goFiles), overlay)
	if err != nil {
		return nil, err
	}
	inputs := make([]transpile.Input, 0, len(fileGroup.inputs))
	for _, source := range fileGroup.inputs {
		src, err := dirInfo.readSource(source.path)
		if err != nil {
			return nil, err
		}
		inputs = append(inputs, transpile.Input{Path: source.path, Src: src})
	}
	goFilePaths := make([]string, 0, len(fileGroup.goFiles))
	for _, source := range fileGroup.goFiles {
		goFilePaths = append(goFilePaths, source.path)
	}
	config := transpile.Config{Importer: importer, GoFiles: goFilePaths, LineDirectives: options.lineDirectives, KeepGoing: options.keepGoing}
	outputs, err := transpile.Transpile(config, inputs)
	return keepEmitted(outputs, fileGroup), err
}

// keepEmitted returns the outputs that the group produces. A group can
// type-check more files than it emits.
func keepEmitted(outputs []transpile.Output, fileGroup group) []transpile.Output {
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
	return kept
}
