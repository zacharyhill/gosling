package main

import (
	"errors"
	"fmt"
	"go/types"

	"golang.org/x/tools/go/packages"
)

// newImporter loads type information for the packages that files import.
// The overlay supplies generated files that can be missing from disk.
func newImporter(dir string, files []sourceFile, overlay map[string][]byte) (types.Importer, error) {
	packagesByPath, err := loadImports(dir, uniqueImports(files), overlay)
	if err != nil {
		return nil, err
	}
	return importerFunc(func(requestedPath string) (*types.Package, error) {
		if requestedPath == "unsafe" {
			return types.Unsafe, nil
		}
		if pkg, ok := packagesByPath[requestedPath]; ok {
			return pkg, nil
		}
		return nil, fmt.Errorf("package %s was not loaded", requestedPath)
	}), nil
}

// uniqueImports lists each path that files import, one time. It skips unsafe
// and C, because they are not loaded as packages.
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
func loadImports(dir string, importPaths []string, overlay map[string][]byte) (map[string]*types.Package, error) {
	packagesByPath := map[string]*types.Package{}
	if len(importPaths) == 0 {
		return packagesByPath, nil
	}
	config := &packages.Config{
		Mode:    packages.NeedName | packages.NeedTypes | packages.NeedImports | packages.NeedDeps,
		Dir:     dir,
		Overlay: overlay,
	}
	loaded, err := packages.Load(config, importPaths...)
	if err != nil {
		return nil, err
	}
	var loadErrors []error
	for _, loadedPkg := range loaded {
		for _, pkgError := range loadedPkg.Errors {
			loadErrors = append(loadErrors, errors.New(pkgError.Error()))
		}
	}
	if len(loadErrors) > 0 {
		return nil, errors.Join(loadErrors...)
	}
	packages.Visit(loaded, nil, func(loadedPkg *packages.Package) {
		packagesByPath[loadedPkg.PkgPath] = loadedPkg.Types
	})
	return packagesByPath, nil
}

type importerFunc func(path string) (*types.Package, error)

func (importFunc importerFunc) Import(path string) (*types.Package, error) { return importFunc(path) }
