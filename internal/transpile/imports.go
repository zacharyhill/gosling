package transpile

import (
	"go/ast"
	"go/token"
	"go/types"
	"sort"
	"strconv"
)

// importName returns the name that file uses for the package at path. It
// adds an import if the file has none. name is the own name of the package.
// If inAST is true, it also adds the import to the AST. The next type-check
// pass needs this to resolve the types. Then Config.Importer must load
// each package that the imports of the file depend on. It reports false if
// the name is already used for something else in the file. If another file
// declares the name at package level, the import gets an alias.
func (transpiler *transpiler) importName(file *file, path, name string, inAST bool) (string, bool) {
	for _, spec := range file.ast.Imports {
		if importPath, err := strconv.Unquote(spec.Path.Value); err != nil || importPath != path {
			continue
		}
		switch {
		case spec.Name == nil:
			return name, true
		case spec.Name.Name == ".":
			return "", true
		case spec.Name.Name != "_":
			return spec.Name.Name, true
		}
	}
	if addedName, ok := file.added[path]; ok && !inAST {
		return addedName, true
	}
	if file.identNames[name] {
		return "", false
	}
	importedAs := name
	if transpiler.isPackageLevelName(name) {
		importedAs = transpiler.pickImportAlias(file, name)
		file.aliased[path] = true
	}
	file.added[path] = importedAs
	if inAST {
		spec := &ast.ImportSpec{Path: &ast.BasicLit{Kind: token.STRING, Value: strconv.Quote(path)}}
		if importedAs != name {
			spec.Name = ast.NewIdent(importedAs)
		}
		file.ast.Decls = append([]ast.Decl{&ast.GenDecl{Tok: token.IMPORT, Specs: []ast.Spec{spec}}}, file.ast.Decls...)
		file.ast.Imports = append(file.ast.Imports, spec)
	}
	return importedAs, true
}

// isPackageLevelName reports whether a file of the package declares name at
// package level.
func (transpiler *transpiler) isPackageLevelName(name string) bool {
	return transpiler.checkedPackage != nil && transpiler.checkedPackage.Scope().Lookup(name) != nil
}

// importAliasSuffix follows the package name in an import alias, as in fmtpkg.
const importAliasSuffix = "pkg"

// pickImportAlias returns an import name based on name that file does not use
// and the package does not declare.
func (transpiler *transpiler) pickImportAlias(file *file, name string) string {
	alias := name + importAliasSuffix
	for suffix := 2; file.identNames[alias] || transpiler.isPackageLevelName(alias); suffix++ {
		alias = name + importAliasSuffix + strconv.Itoa(suffix)
	}
	return alias
}

// importNameAt is importName for code at position. It also reports false
// if a declaration in scope hides the name there.
func (lowerer *lowerer) importNameAt(position token.Pos, path, name string) (string, bool) {
	packageName, ok := lowerer.importName(lowerer.file, path, name, false)
	return packageName, ok && !isHiddenAt(lowerer.scopeAt(position), position, packageName, path)
}

// isHiddenAt reports whether a declaration in scope hides packageName, the
// name of the package at path, at position.
func isHiddenAt(scope *types.Scope, position token.Pos, packageName, path string) bool {
	if scope == nil || packageName == "" {
		return false
	}
	_, object := scope.LookupParent(packageName, position)
	if object == nil {
		return false
	}
	importedPackage, isPackage := object.(*types.PkgName)
	return !isPackage || importedPackage.Imported().Path() != path
}

func (lowerer *lowerer) emitImports() {
	paths := make([]string, 0, len(lowerer.file.added))
	for path := range lowerer.file.added {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var lastImport *ast.GenDecl
	for _, decl := range lowerer.file.ast.Decls {
		if genDecl, ok := decl.(*ast.GenDecl); ok && genDecl.Tok == token.IMPORT && genDecl.Pos().IsValid() {
			lastImport = genDecl
		}
	}
	for _, path := range paths {
		quoted := strconv.Quote(path)
		if lowerer.file.aliased[path] {
			quoted = lowerer.file.added[path] + " " + quoted
		}
		switch {
		case lastImport == nil:
			lowerer.insert(lowerer.file.ast.Name.End(), "\n\nimport "+quoted, lowerer.line(lowerer.file.ast.Name.End()))
		case lastImport.Rparen.IsValid() && !lowerer.config.LineDirectives:
			// gofmt sorts the block. With line directives, use a separate
			// declaration, because sorting moves the directives.
			lowerer.insert(lastImport.Rparen, quoted+"\n", lowerer.line(lastImport.Rparen))
		default:
			lowerer.insert(lastImport.End(), "\nimport "+quoted, lowerer.line(lastImport.End()))
		}
	}
}
