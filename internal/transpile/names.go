package transpile

import (
	"fmt"
	"go/token"
	"go/types"
	"strconv"
)

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
		if !lowerer.taken(candidate.catchLabel()) && !lowerer.taken(candidate.endLabel()) && !lowerer.taken(candidate.finallyLabel()) {
			return candidate.id
		}
	}
}

// pickErrorVar picks a name for a generated error variable. The name must
// not be a catch variable of an enclosing try block or a package-level name.
// With a scope, := declares the name at position. So the name must reuse an
// earlier error variable of that scope, or not occur later in the scope. If
// end is valid, the name must not occur in [position, end).
func (lowerer *lowerer) pickErrorVar(try *tryContext, scope *types.Scope, position, end token.Pos) string {
	for attempt := 1; ; attempt++ {
		name := "err"
		if attempt > 1 {
			name = fmt.Sprintf("err%d", attempt)
		}
		if excludedByTry(try, name) || lowerer.isPackageLevelName(name) {
			continue
		}
		if scope != nil {
			canReuse, isFree := lowerer.fitsScope(name, scope, position)
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
func (lowerer *lowerer) fitsScope(name string, scope *types.Scope, position token.Pos) (canReuse, isFree bool) {
	if object := scope.Lookup(name); object != nil {
		return object.Pos() < position && isErrorVar(object), false
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

func isErrorVar(object types.Object) bool {
	variable, ok := object.(*types.Var)
	return ok && types.Identical(variable.Type(), errorType)
}

// pickTempVar returns a temporary name not referenced in [from, to) and
// not declared at package level.
func (lowerer *lowerer) pickTempVar(from, to token.Pos) string {
	for attempt := 1; ; attempt++ {
		name := "v"
		if attempt > 1 {
			name = "v" + strconv.Itoa(attempt)
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
