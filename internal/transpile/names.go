package transpile

import (
	"fmt"
	"go/token"
	"go/types"
	"strconv"
)

func (lowerer *lowerer) scopeAt(pos token.Pos) *types.Scope {
	if scope := lowerer.info.Scopes[lowerer.file.ast]; scope != nil {
		return scope.Innermost(pos)
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

// fresh returns an identifier not used anywhere in the file.
func (lowerer *lowerer) fresh(base string) string {
	name := base
	for suffix := 2; lowerer.taken(name); suffix++ {
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
// not be a catch variable of an enclosing try block. With a scope, := declares
// the name at pos. So the name must reuse an earlier error variable of that
// scope, or not occur later in the scope. If end is valid, the name must not
// occur in [pos, end).
func (lowerer *lowerer) pickErrorVar(try *tryContext, scope *types.Scope, pos, end token.Pos) string {
	for attempt := 1; ; attempt++ {
		name := "err"
		if attempt > 1 {
			name = fmt.Sprintf("err%d", attempt)
		}
		if excludedByTry(try, name) {
			continue
		}
		if scope != nil {
			if object := scope.Lookup(name); object != nil {
				if object.Pos() < pos && isErrorVar(object) {
					return name
				}
				continue
			}
			if _, ok := lowerer.usedIn(name, pos, scope.End()); ok {
				continue
			}
		}
		if end.IsValid() {
			if _, ok := lowerer.usedIn(name, pos, end); ok {
				continue
			}
		}
		return name
	}
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

// pickTempVar returns a temporary name not referenced in [from, to).
func (lowerer *lowerer) pickTempVar(from, to token.Pos) string {
	for attempt := 1; ; attempt++ {
		name := "v"
		if attempt > 1 {
			name = "v" + strconv.Itoa(attempt)
		}
		if _, used := lowerer.usedIn(name, from, to); !used {
			return name
		}
	}
}
