package preprocess

import (
	"bytes"
	"fmt"
	"go/token"
	"slices"
	"strings"
)

// binding is one name in a destructuring pattern. key is the source field
// and name is the target variable.
type binding struct {
	key          string // field name, not used for array patterns
	name         string
	patternIndex int
	rest         bool
}

// destructure is a destructuring statement with a right side that the scan
// has not finished. text is the binding assignment to insert where the right
// side isEnd.
type destructure struct {
	start nesting
	text  string
	// newlines are the newlines of a multi-line pattern. They follow text.
	newlines string
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
	if isArray {
		// A blank element reads no index, so it cannot panic.
		bindings = slices.DeleteFunc(bindings, func(elementBinding binding) bool { return elementBinding.name == "_" })
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

	if end, ok := processor.simpleExpr(operatorIndex + 1); ok {
		rightSide := string(processor.src[processor.tokenAt(operatorIndex+1).offset:processor.tokenAt(end).end()])
		assignText := formatAssignment(bindings, operatorText, rightSide, isArray)
		newlines := processor.collectNewlines(open.offset, processor.tokenAt(end).end())
		namesAt := processor.replace(open.offset, processor.tokenAt(end).end(), assignText+newlines)
		processor.recordNewlines(namesAt+len(assignText), newlines)
		processor.recordConsts(namesAt, constBindings)
		return end, true, nil
	}
	// Other right sides go into a temporary variable, so they run once. The
	// scan then continues into the right side and rewrites its gosling syntax.
	rightSideVar := processor.fresh("tmp")
	processor.result.TempNames = append(processor.result.TempNames, rightSideVar)
	processor.replace(open.offset, operator.end(), rightSideVar+" :=")
	processor.pendingDestructures = append(processor.pendingDestructures, &destructure{
		start:         processor.nesting(),
		text:          "; " + formatAssignment(bindings, operatorText, rightSideVar, isArray),
		newlines:      processor.collectNewlines(open.offset, operator.end()),
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
			afterIndex, isClosed := processor.findPatternEnd(tokenIndex+2, closing)
			if !processor.tokenAt(tokenIndex+1).is(token.IDENT) || !isClosed {
				return nil, 0, nil
			}
			bindings = append(bindings, binding{name: processor.tokenAt(tokenIndex + 1).text, patternIndex: len(bindings), rest: true})
			return bindings, afterIndex, nil
		}
		if !current.is(token.IDENT) {
			return nil, 0, nil
		}
		newBinding := binding{key: current.text, name: current.text, patternIndex: len(bindings)}
		tokenIndex++
		if !isArray && processor.tokenAt(tokenIndex).is(token.COLON) && processor.tokenAt(tokenIndex+1).is(token.IDENT) {
			newBinding.name = processor.tokenAt(tokenIndex + 1).text
			tokenIndex += 2
		}
		bindings = append(bindings, newBinding)
		if afterIndex, isClosed := processor.findPatternEnd(tokenIndex, closing); isClosed {
			return bindings, afterIndex, nil
		}
		if !processor.tokenAt(tokenIndex).is(token.COMMA) {
			return nil, 0, nil
		}
		tokenIndex++
	}
}

// findPatternEnd reports whether the tokens at tokenIndex close a pattern: an
// optional trailing comma, the newline before the closing bracket, and the
// closing bracket. It returns the index after the closing bracket.
func (processor *processor) findPatternEnd(tokenIndex int, closing token.Token) (int, bool) {
	if processor.tokenAt(tokenIndex).is(token.COMMA) {
		tokenIndex++
	}
	if processor.tokenAt(tokenIndex).isAutoSemicolon() {
		tokenIndex++
	}
	return tokenIndex + 1, processor.tokenAt(tokenIndex).is(closing)
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

// formatAssignment returns the statement that assigns the bindings from rightSide.
// Without bindings, it only evaluates rightSide.
func formatAssignment(bindings []binding, operatorText, rightSide string, isArray bool) string {
	if len(bindings) == 0 {
		return "_ = " + rightSide
	}
	return names(bindings) + " " + operatorText + " " + values(bindings, rightSide, isArray)
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
			sources[bindingIndex] = fmt.Sprintf("%s[%d:]", rightSide, binding.patternIndex)
		case isArray:
			sources[bindingIndex] = fmt.Sprintf("%s[%d]", rightSide, binding.patternIndex)
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
	insertedAt := processor.replace(end, end, pending.text+pending.newlines)
	processor.recordNewlines(insertedAt+len(pending.text), pending.newlines)
	processor.result.Splits = append(processor.result.Splits, insertedAt)
	processor.recordConsts(insertedAt+len("; "), pending.constBindings)
	// A function literal on the right side can contain another destructuring
	// that isEnd at the same token.
	return processor.finishDestructure(tokenIndex)
}

func (processor *processor) collectNewlines(start, end int) string {
	return strings.Repeat("\n", bytes.Count(processor.src[start:end], []byte("\n")))
}

func (processor *processor) recordNewlines(offset int, newlines string) {
	if newlines != "" {
		processor.result.Newlines = append(processor.result.Newlines, [2]int{offset, offset + len(newlines)})
	}
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
