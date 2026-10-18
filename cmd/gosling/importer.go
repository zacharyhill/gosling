package main

import (
	"errors"
	"fmt"
	"go/build"
	"go/types"

	"golang.org/x/tools/go/packages"
)

// newImporter loads type information for the packages that files import,
// with the build configuration context. The overlay supplies generated
// files that can be missing from disk.
func newImporter(dir string, files []sourceFile, overlay map[string][]byte, context build.Context) (types.Importer, error) {
	packagesByPath, err := loadImports(dir, uniqueImports(files), overlay, context)
	if err != nil {
		return nil, err
	}
	return importerFunc(func(requestedPath string) (*types.Package, error) {
		if requestedPath == "unsafe" {
			return types.Unsafe, nil
		}
		if typesPackage, ok := packagesByPath[requestedPath]; ok {
			return typesPackage, nil
		}
		return nil, fmt.Errorf("package %s was not loaded", requestedPath)
	}), nil
}

// uniqueImports lists each path that files import, one time. It skips unsafe
// and C, because gosling does not load them as packages.
func uniqueImports(files []sourceFile) []string {
	seen := map[string]bool{"unsafe": true, "C": true}
	var importPaths []string
	for _, source := range files {
		for _, importedPath := range source.imports {
			if !seen[importedPath] {
				seen[importedPath] = true
				importPaths = append(importPaths, importedPath)
			}
		}
	}
	return importPaths
}

// loadImports type-checks importPaths and all their dependencies. To name a
// type, gosling can import a package that it reaches only through one of
// these.
func loadImports(dir string, importPaths []string, overlay map[string][]byte, context build.Context) (map[string]*types.Package, error) {
	packagesByPath := map[string]*types.Package{}
	if len(importPaths) == 0 {
		return packagesByPath, nil
	}
	env, buildFlags := buildContextEnv(context)
	config := &packages.Config{
		Mode:       packages.NeedName | packages.NeedTypes | packages.NeedImports | packages.NeedDeps,
		Dir:        dir,
		Env:        env,
		BuildFlags: buildFlags,
		Overlay:    overlay,
	}
	loaded, err := packages.Load(config, importPaths...)
	if err != nil {
		return nil, err
	}
	var loadErrors []error
	for _, loadedPackage := range loaded {
		for _, packageError := range loadedPackage.Errors {
			loadErrors = append(loadErrors, errors.New(packageError.Error()))
		}
	}
	if len(loadErrors) > 0 {
		return nil, errors.Join(loadErrors...)
	}
	packages.Visit(loaded, nil, func(loadedPackage *packages.Package) {
		packagesByPath[loadedPackage.PkgPath] = loadedPackage.Types
	})
	return packagesByPath, nil
}

type importerFunc func(path string) (*types.Package, error)

func (importerFunc importerFunc) Import(path string) (*types.Package, error) {
	return importerFunc(path)
}
