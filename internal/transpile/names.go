package transpile

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"slices"
	"strconv"
)

// avoidPackageNamesInTemps preprocesses a file again if a temporary variable
// can hide a package-level name. Temporaries are local, so one pass is enough.
func (transpiler *transpiler) avoidPackageNamesInTemps() error {
	astFiles := slices.Clone(transpiler.plainGoFiles)
	for _, golFile := range transpiler.files {
		astFiles = append(astFiles, golFile.ast)
	}
	packageNames := collectPackageLevelNames(astFiles)
	isPackageName := func(name string) bool { return packageNames[name] }
	for fileIndex, golFile := range transpiler.files {
		if !slices.ContainsFunc(golFile.preprocessed.TempNames, isPackageName) {
			continue
		}
		rebuilt, err := transpiler.buildFile(Input{Path: golFile.path, Src: golFile.original}, packageNames)
		if err != nil {
			return err
		}
		if rebuilt != nil {
			transpiler.files[fileIndex] = rebuilt
		}
	}
	return nil
}

// collectPackageLevelNames returns the names that astFiles declare at package level.
func collectPackageLevelNames(astFiles []*ast.File) map[string]bool {
	names := map[string]bool{}
	for _, astFile := range astFiles {
		for _, decl := range astFile.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if decl.Recv == nil {
					names[decl.Name.Name] = true
				}
			case *ast.GenDecl:
				addGenDeclNames(names, decl)
			}
		}
	}
	return names
}

// addGenDeclNames adds the type, const, and var names that genDecl declares.
func addGenDeclNames(names map[string]bool, genDecl *ast.GenDecl) {
	for _, spec := range genDecl.Specs {
		switch spec := spec.(type) {
		case *ast.TypeSpec:
			names[spec.Name.Name] = true
		case *ast.ValueSpec:
			for _, name := range spec.Names {
				names[name.Name] = true
			}
		}
	}
}

func (lowerer *lowerer) scopeAt(position token.Pos) *types.Scope {
	if scope := lowerer.typeInfo.Scopes[lowerer.file.ast]; scope != nil {
		return scope.Innermost(position)
	}
	return nil
}

// usedIn returns the first position of an identifier named name in [from, to).
func (lowerer *lowerer) usedIn(name string, from, to token.Pos) (token.Pos, bool) {
	for _, identPos := range lowerer.identPositions[name] {
		if identPos >= from && identPos < to {
			return identPos, true
		}
	}
	return token.NoPos, false
}

func (lowerer *lowerer) taken(name string) bool { return len(lowerer.identPositions[name]) > 0 }

// fresh returns an identifier not used anywhere in the file and not
// declared at package level.
func (lowerer *lowerer) fresh(base string) string {
	name := base
	for suffix := 2; lowerer.taken(name) || lowerer.isPackageLevelName(name); suffix++ {
		name = fmt.Sprintf("%s%d", base, suffix)
	}
	lowerer.identPositions[name] = append(lowerer.identPositions[name], token.NoPos)
	return name
}

// nextTryID returns an id whose labels do not clash with an identifier in the file.
func (lowerer *lowerer) nextTryID() int {
	for {
		lowerer.lastTryID++
		candidate := &tryContext{id: lowerer.lastTryID}
		if !lowerer.taken(candidate.catchLabel()) && !lowerer.taken(candidate.endLabel()) && !lowerer.taken(candidate.finallyLabel()) && !lowerer.taken(candidate.panicLabel()) {
			return candidate.id
		}
	}
}

// pickErrorVar picks a name for a generated error variable. The name must
// not be a catch variable of an enclosing try block or a package-level name.
// With a scope, := declares the name at position. So the name must reuse an
// earlier error variable of that scope, or not occur later in the scope. If
// end is valid, the name must not occur in [position, end). An earlier
// variable is reused only if it has the type errorResult.
func (lowerer *lowerer) pickErrorVar(try *tryContext, scope *types.Scope, position, end token.Pos, errorResult types.Type) string {
	for attempt := 1; ; attempt++ {
		name := "err"
		if attempt > 1 {
			name = fmt.Sprintf("err%d", attempt)
		}
		if excludedByTry(try, name) || lowerer.isPackageLevelName(name) {
			continue
		}
		if scope != nil {
			canReuse, isFree := lowerer.fitsScope(name, scope, position, errorResult)
			if canReuse {
				return name
			}
			if !isFree {
				continue
			}
		}
		if _, ok := lowerer.usedIn(name, position, end); end.IsValid() && ok {
			continue
		}
		return name
	}
}

// fitsScope reports whether := at position can reuse name as an error
// variable of scope. It also reports whether name is free in the rest of scope.
func (lowerer *lowerer) fitsScope(name string, scope *types.Scope, position token.Pos, errorResult types.Type) (canReuse, isFree bool) {
	if object := scope.Lookup(name); object != nil {
		return object.Pos() < position && isVarOfType(object, errorResult), false
	}
	// A bare return cannot see a named result that := shadows.
	if _, outerObject := scope.LookupParent(name, position); lowerer.namedResults[outerObject] {
		return false, false
	}
	_, isUsed := lowerer.usedIn(name, position, scope.End())
	return false, !isUsed
}

func excludedByTry(try *tryContext, name string) bool {
	for enclosing := try; enclosing != nil; enclosing = enclosing.outer {
		if enclosing.catchVar == name {
			return true
		}
	}
	return false
}

func isVarOfType(object types.Object, varType types.Type) bool {
	variable, ok := object.(*types.Var)
	return ok && types.Identical(variable.Type(), varType)
}

// pickTempVar returns a temporary name not referenced in [from, to) and
// not declared at package level.
func (lowerer *lowerer) pickTempVar(from, to token.Pos) string {
	return lowerer.pickName("v", from, to)
}

// pickName returns base, or base with a number, not referenced in [from, to).
func (lowerer *lowerer) pickName(base string, from, to token.Pos) string {
	for attempt := 1; ; attempt++ {
		name := base
		if attempt > 1 {
			name = base + strconv.Itoa(attempt)
		}
		if _, used := lowerer.usedIn(name, from, to); !used && !lowerer.isPackageLevelName(name) {
			return name
		}
	}
}

// checkBuiltins reports an error and returns false if a declaration at position
// shadows one of names. The code that gosling generates there uses these builtins.
func (lowerer *lowerer) checkBuiltins(position token.Pos, construct string, names ...string) bool {
	scope := lowerer.scopeAt(position)
	if scope == nil {
		return true
	}
	for _, name := range names {
		if _, object := scope.LookupParent(name, position); object != nil && object.Parent() != types.Universe {
			lowerer.errorf(lowerer.file, position, "%s needs the builtin %s, but a declaration shadows it here; rename that declaration", construct, name)
			return false
		}
	}
	return true
}
