package preprocess

import (
	"fmt"
	"go/token"
	"strings"
)

// binding is one name in a destructuring pattern. key is the source field
// and name is the target variable.
type binding struct {
	key        string // field name, not used for array patterns
	name       string
	rest       bool
	nameOffset int
}

// destructure is a destructuring statement with a right side that the scan
// has not finished. text is the binding assignment to insert where the right
// side isEnd.
type destructure struct {
	start nesting
	text  string
	// constBindings holds the bindings to record in Result.Consts.
	constBindings []binding
}

// destructure recognizes `{a, b: c} := x` and `[a, b, ...rest] := x` at
// token tokenIndex. keyword is the let or const before the pattern, or nil.
// It reports whether the tokens are a pattern.
func (processor *processor) destructure(tokenIndex int, keyword *lexeme) (int, bool, error) {
	open := processor.tokens[tokenIndex]
	isArray := open.is(token.LBRACK)
	bindings, operatorIndex, err := processor.pattern(tokenIndex+1, isArray)
	if err != nil || bindings == nil {
		return tokenIndex, false, err
	}
	operator := processor.tokenAt(operatorIndex)
	if !operator.is(token.DEFINE) && !operator.is(token.ASSIGN) {
		return tokenIndex, false, nil
	}
	operatorText := operator.kind.String()
	var constBindings []binding
	if keyword != nil {
		if operator.is(token.DEFINE) {
			return tokenIndex, false, &Error{keyword.offset, fmt.Sprintf("use %s {...} = value, not :=", keyword.text)}
		}
		operatorText = ":="
		if keyword.is(token.CONST) {
			constBindings = bindings
		}
	}

	if isArray && processor.isCommaOK(bindings, operatorIndex+1) {
		namesAt := processor.rewriteCommaOK(open, operator, operatorText, bindings)
		processor.recordConsts(namesAt, constBindings)
		return operatorIndex, true, nil
	}
	if end, ok := processor.simpleExpr(operatorIndex + 1); ok {
		rightSide := string(processor.src[processor.tokenAt(operatorIndex+1).offset:processor.tokenAt(end).end()])
		namesAt := processor.replace(open.offset, processor.tokenAt(end).end(), names(bindings)+" "+operatorText+" "+values(bindings, rightSide, isArray))
		processor.recordConsts(namesAt, constBindings)
		return end, true, nil
	}
	// Other right sides go into a temporary variable, so they run once. The
	// scan then continues into the right side and rewrites its gosling syntax.
	rightSideVar := processor.fresh("tmp")
	processor.replace(open.offset, operator.end(), rightSideVar+" :=")
	processor.pendingDestructures = append(processor.pendingDestructures, &destructure{
		start:         processor.nesting(),
		text:          "; " + names(bindings) + " " + operatorText + " " + values(bindings, rightSideVar, isArray),
		constBindings: constBindings,
	})
	return operatorIndex, true, nil
}

// pattern parses the bindings that start at token tokenIndex, after the
// opening bracket. It returns nil bindings if the tokens are not a pattern.
func (processor *processor) pattern(tokenIndex int, isArray bool) ([]binding, int, error) {
	closing := token.RBRACE
	if isArray {
		closing = token.RBRACK
	}
	var bindings []binding
	for {
		current := processor.tokenAt(tokenIndex)
		if current.is(token.ELLIPSIS) {
			if !isArray {
				return nil, 0, &Error{current.offset, "rest elements are only supported in array destructuring"}
			}
			if !processor.tokenAt(tokenIndex+1).is(token.IDENT) || !processor.tokenAt(tokenIndex+2).is(closing) {
				return nil, 0, nil
			}
			name := processor.tokenAt(tokenIndex + 1)
			bindings = append(bindings, binding{name: name.text, rest: true, nameOffset: name.offset})
			return bindings, tokenIndex + 3, nil
		}
		if !current.is(token.IDENT) {
			return nil, 0, nil
		}
		newBinding := binding{key: current.text, name: current.text, nameOffset: current.offset}
		tokenIndex++
		if !isArray && processor.tokenAt(tokenIndex).is(token.COLON) && processor.tokenAt(tokenIndex+1).is(token.IDENT) {
			newBinding.name, newBinding.nameOffset = processor.tokenAt(tokenIndex+1).text, processor.tokenAt(tokenIndex+1).offset
			tokenIndex += 2
		}
		bindings = append(bindings, newBinding)
		switch next := processor.tokenAt(tokenIndex); {
		case next.is(closing):
			return bindings, tokenIndex + 1, nil
		case next.is(token.COMMA):
			tokenIndex++
		default:
			return nil, 0, nil
		}
	}
}

// isCommaOK reports whether `[v, ok] = x` is a comma-ok form. The pattern
// has two names, and the right side that starts at token tokenIndex is an
// index expression, a type assertion, or a receive.
func (processor *processor) isCommaOK(bindings []binding, tokenIndex int) bool {
	if len(bindings) != 2 || bindings[1].rest {
		return false
	}
	isReceive := processor.tokenAt(tokenIndex).is(token.ARROW)
	if isReceive {
		tokenIndex++
	}
	lastOperation, ok := processor.findLastPostfix(tokenIndex)
	return ok && (isReceive || lastOperation == token.LBRACK || lastOperation == token.TYPE)
}

// findLastPostfix scans the operand and postfix operations that start at token
// tokenIndex, such as a.b[i].(T), to the end of the statement. It returns
// the kind of the last operation: token.LBRACK for an index, token.TYPE for
// a type assertion, or token.ILLEGAL for others. It reports false if the
// statement has more than that.
func (processor *processor) findLastPostfix(tokenIndex int) (token.Token, bool) {
	if !processor.tokenAt(tokenIndex).is(token.IDENT) {
		return token.ILLEGAL, false
	}
	lastOperation := token.ILLEGAL
	for tokenIndex++; ; {
		current, next := processor.tokenAt(tokenIndex), processor.tokenAt(tokenIndex+1)
		switch {
		case current.is(token.SEMICOLON) || current.is(token.RBRACE) || current.is(token.EOF):
			return lastOperation, true
		case current.is(token.PERIOD) && next.is(token.IDENT):
			lastOperation, tokenIndex = token.ILLEGAL, tokenIndex+2
		case current.is(token.PERIOD) && next.is(token.LPAREN):
			closeIndex, _ := processor.matchParen(tokenIndex + 1)
			if closeIndex < 0 || processor.tokenAt(tokenIndex+2).is(token.TYPE) {
				return token.ILLEGAL, false
			}
			lastOperation, tokenIndex = token.TYPE, closeIndex+1
		case current.is(token.LPAREN):
			closeIndex, _ := processor.matchParen(tokenIndex)
			if closeIndex < 0 {
				return token.ILLEGAL, false
			}
			lastOperation, tokenIndex = token.ILLEGAL, closeIndex+1
		case current.is(token.LBRACK):
			closeIndex, isSlice := processor.matchIndex(tokenIndex)
			if closeIndex < 0 {
				return token.ILLEGAL, false
			}
			lastOperation, tokenIndex = token.LBRACK, closeIndex+1
			if isSlice {
				lastOperation = token.ILLEGAL
			}
		default:
			return token.ILLEGAL, false
		}
	}
}

// matchIndex returns the index of the ] that closes the [ at token
// lbrackIndex. It also reports whether a top-level colon makes it a slice
// expression.
func (processor *processor) matchIndex(lbrackIndex int) (int, bool) {
	depth, isSlice := 0, false
	for tokenIndex := lbrackIndex; tokenIndex < len(processor.tokens); tokenIndex++ {
		switch current := processor.tokens[tokenIndex]; {
		case current.is(token.LPAREN) || current.is(token.LBRACK) || current.is(token.LBRACE):
			depth++
		case current.is(token.RPAREN) || current.is(token.RBRACK) || current.is(token.RBRACE):
			if depth--; depth == 0 {
				return tokenIndex, isSlice
			}
		case current.is(token.COLON) && depth == 1:
			isSlice = true
		}
	}
	return -1, false
}

// rewriteCommaOK rewrites `[v, ok] = x` into `v, ok := x`. The names stay source
// text. The scan then continues into the right side. It returns the output
// offset of the names.
func (processor *processor) rewriteCommaOK(open, operator lexeme, operatorText string, bindings []binding) int {
	first, second := bindings[0], bindings[1]
	namesAt := processor.replace(open.offset, first.nameOffset, "")
	processor.replace(first.nameOffset+len(first.name), second.nameOffset, ", ")
	processor.replace(second.nameOffset+len(second.name), operator.end(), " "+operatorText)
	processor.result.CommaOK = append(processor.result.CommaOK, namesAt)
	return namesAt
}

// simpleExpr reports whether the tokens from tokenIndex are `a` or `a.b.c` at
// the end of the statement. Such an expression is safe to repeat. It returns
// the index of the last token.
func (processor *processor) simpleExpr(tokenIndex int) (int, bool) {
	if !processor.tokenAt(tokenIndex).is(token.IDENT) {
		return 0, false
	}
	for processor.tokenAt(tokenIndex+1).is(token.PERIOD) && processor.tokenAt(tokenIndex+2).is(token.IDENT) {
		tokenIndex += 2
	}
	switch processor.tokenAt(tokenIndex + 1).kind {
	case token.SEMICOLON, token.RBRACE, token.EOF:
		return tokenIndex, true
	}
	return 0, false
}

func names(bindings []binding) string {
	targets := make([]string, len(bindings))
	for bindingIndex, binding := range bindings {
		targets[bindingIndex] = binding.name
	}
	return strings.Join(targets, ", ")
}

func values(bindings []binding, rightSide string, isArray bool) string {
	sources := make([]string, len(bindings))
	for bindingIndex, binding := range bindings {
		switch {
		case binding.rest:
			sources[bindingIndex] = fmt.Sprintf("%s[%d:]", rightSide, bindingIndex)
		case isArray:
			sources[bindingIndex] = fmt.Sprintf("%s[%d]", rightSide, bindingIndex)
		default:
			sources[bindingIndex] = rightSide + "." + binding.key
		}
	}
	return strings.Join(sources, ", ")
}

// finishDestructure inserts the pending binding assignment if token
// tokenIndex isEnd the right side of the destructuring statement.
func (processor *processor) finishDestructure(tokenIndex int) error {
	if len(processor.pendingDestructures) == 0 {
		return nil
	}
	pending := processor.pendingDestructures[len(processor.pendingDestructures)-1]
	current := processor.tokenAt(tokenIndex)
	isEnd := current.is(token.EOF) || ((current.is(token.SEMICOLON) || current.is(token.RBRACE)) && processor.nesting() == pending.start)
	if !isEnd {
		return nil
	}
	processor.pendingDestructures = processor.pendingDestructures[:len(processor.pendingDestructures)-1]
	end := processor.tokenAt(tokenIndex - 1).end()
	insertedAt := processor.replace(end, end, pending.text)
	processor.result.Splits = append(processor.result.Splits, insertedAt)
	processor.recordConsts(insertedAt+len("; "), pending.constBindings)
	// A function literal on the right side can contain another destructuring
	// that isEnd at the same token.
	return processor.finishDestructure(tokenIndex)
}

// recordConsts adds the offsets of the named bindings in a names(bindings)
// list that starts at offset.
func (processor *processor) recordConsts(offset int, bindings []binding) {
	for _, binding := range bindings {
		if binding.name != "_" {
			processor.result.Consts = append(processor.result.Consts, offset)
		}
		offset += len(binding.name) + len(", ")
	}
}
