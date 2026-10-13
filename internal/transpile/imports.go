package transpile

import (
	"go/ast"
	"go/token"
	"sort"
	"strconv"
)

// importName returns the name that file uses for the package at path. It
// adds an import if the file has none. name is the own name of the package.
// If inAST is true, it also adds the import to the AST. The next type-check
// pass needs this to resolve the types. Then Config.Importer must load
// each package that the imports of the file depend on. It reports false if
// the name is already used for something else in the file.
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
	file.added[path] = name
	if inAST {
		spec := &ast.ImportSpec{Path: &ast.BasicLit{Kind: token.STRING, Value: strconv.Quote(path)}}
		file.ast.Decls = append([]ast.Decl{&ast.GenDecl{Tok: token.IMPORT, Specs: []ast.Spec{spec}}}, file.ast.Decls...)
		file.ast.Imports = append(file.ast.Imports, spec)
	}
	return name, true
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
