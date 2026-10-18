package main

import (
	"errors"
	"fmt"
	"go/build"
	"path/filepath"
	"slices"
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
// package, so gosling type-checks them with its .gol files. A .gol file of
// another package that the build skips, such as a main package with
// "//go:build ignore", gets its own group.
func (packageDir *packageDir) groups() ([]group, error) {
	packageName, err := packageDir.packageName()
	if err != nil {
		return nil, err
	}
	var golRoles, goRoles filesByRole
	var standaloneGroups []group
	for _, source := range packageDir.golFiles {
		if !source.isInDefaultBuild && !fitsPackage(source, packageName) {
			standaloneGroups = append(standaloneGroups, group{inputs: []sourceFile{source}, emit: []sourceFile{source}})
			continue
		}
		if err := golRoles.add(source, packageName); err != nil {
			return nil, err
		}
	}
	for _, source := range packageDir.goFiles {
		if !source.isInDefaultBuild && !fitsPackage(source, packageName) {
			continue
		}
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
	return append(groups, standaloneGroups...), nil
}

func fitsPackage(source sourceFile, packageName string) bool {
	return source.packageName == packageName || (isTestFile(source) && source.packageName == packageName+"_test")
}

// packageName returns the package the directory's non-test files declare.
// Only files that match build.Default count, if there are any.
func (packageDir *packageDir) packageName() (string, error) {
	allFiles := concatFiles(packageDir.golFiles, packageDir.goFiles)
	if defaultFiles := slices.DeleteFunc(slices.Clone(allFiles), func(source sourceFile) bool { return !source.isInDefaultBuild }); len(defaultFiles) > 0 {
		allFiles = defaultFiles
	}
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

// transpileGroup converts the files of one group. It type-checks the files
// of each build configuration of the group separately. With keepGoing, it
// can return outputs together with an error.
func transpileGroup(dirInfo *packageDir, fileGroup group, overlay map[string][]byte, options transpileOptions) ([]transpile.Output, error) {
	var outputs []transpile.Output
	var contextErrors []error
	for _, context := range collectEmitContexts(fileGroup.emit) {
		contextOutputs, err := transpileInContext(dirInfo, fileGroup, context, overlay, options)
		outputs = append(outputs, contextOutputs...)
		if err != nil {
			if !options.keepGoing {
				return nil, err
			}
			contextErrors = append(contextErrors, err)
		}
	}
	return outputs, errors.Join(contextErrors...)
}

// collectEmitContexts returns the build configurations of files, each one time.
func collectEmitContexts(files []sourceFile) []build.Context {
	var contexts []build.Context
	seen := map[string]bool{}
	for _, source := range files {
		if key := formatContextKey(source.context); !seen[key] {
			seen[key] = true
			contexts = append(contexts, source.context)
		}
	}
	return contexts
}

// transpileInContext converts the files of a group whose build
// configuration is context. It type-checks them with the other files of the
// group that match context.
func transpileInContext(dirInfo *packageDir, fileGroup group, context build.Context, overlay map[string][]byte, options transpileOptions) ([]transpile.Output, error) {
	inputs, inputFiles, err := selectGolInputs(dirInfo, fileGroup.inputs, context)
	if err != nil {
		return nil, err
	}
	goFiles, err := selectGoFiles(dirInfo, fileGroup.goFiles, context)
	if err != nil {
		return nil, err
	}
	var goFilePaths []string
	for _, source := range goFiles {
		goFilePaths = append(goFilePaths, source.path)
	}
	importer, err := newImporter(dirInfo.dir, concatFiles(inputFiles, goFiles), overlay, context)
	if err != nil {
		return nil, err
	}
	config := transpile.Config{Importer: importer, GoFiles: goFilePaths, LineDirectives: options.lineDirectives, KeepGoing: options.keepGoing}
	outputs, err := transpile.Transpile(config, inputs)
	return keepEmitted(outputs, fileGroup.emit, context), err
}

// selectGolInputs reads the .gol files that match context.
func selectGolInputs(dirInfo *packageDir, golFiles []sourceFile, context build.Context) ([]transpile.Input, []sourceFile, error) {
	var inputs []transpile.Input
	var inputFiles []sourceFile
	for _, source := range golFiles {
		src, err := dirInfo.readSource(source.path)
		if err != nil {
			return nil, nil, err
		}
		matches, err := matchesGol(context, source.path, src)
		if err != nil {
			return nil, nil, err
		}
		if matches {
			inputs = append(inputs, transpile.Input{Path: source.path, Src: src})
			inputFiles = append(inputFiles, source)
		}
	}
	return inputs, inputFiles, nil
}

func selectGoFiles(dirInfo *packageDir, goFiles []sourceFile, context build.Context) ([]sourceFile, error) {
	var matching []sourceFile
	for _, source := range goFiles {
		matches, err := context.MatchFile(dirInfo.dir, filepath.Base(source.path))
		if err != nil {
			return nil, err
		}
		if matches {
			matching = append(matching, source)
		}
	}
	return matching, nil
}

// keepEmitted returns the outputs of the files in emit whose build
// configuration is context. A group can type-check more files than it
// emits.
func keepEmitted(outputs []transpile.Output, emit []sourceFile, context build.Context) []transpile.Output {
	wantedPaths := map[string]bool{}
	for _, source := range emit {
		if formatContextKey(source.context) == formatContextKey(context) {
			wantedPaths[transpile.OutputPath(source.path)] = true
		}
	}
	kept := outputs[:0]
	for _, output := range outputs {
		if wantedPaths[output.Path] {
			kept = append(kept, output)
		}
	}
	return kept
}
