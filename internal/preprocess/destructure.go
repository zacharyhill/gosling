package preprocess

import (
	"bytes"
	"fmt"
	"go/token"
	"slices"
	"strings"
)

// ArrayTemp is the temporary of a flat array pattern without a rest element.
type ArrayTemp struct {
	Offset int      // offset of the temporary name
	Names  []string // the element names, with "_" for a blank element
}

// binding is one name in a destructuring pattern. path is the access that
// reads its value from the right side, like ".Addr.Host" or "[0][1:]".
// offset is the offset of the name in the source.
type binding struct {
	name   string
	path   string
	offset int
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
	bindings, operatorIndex, err := processor.pattern(tokenIndex+1, isArray, "")
	if err != nil || bindings == nil {
		return tokenIndex, false, err
	}
	operator := processor.tokenAt(operatorIndex)
	if !operator.is(token.DEFINE) && !operator.is(token.ASSIGN) {
		return tokenIndex, false, nil
	}
	elementNames := listFlatElementNames(bindings)
	// A blank element reads no index, so it cannot panic.
	bindings = slices.DeleteFunc(bindings, func(elementBinding binding) bool {
		return elementBinding.name == "_" && strings.HasSuffix(elementBinding.path, "]")
	})
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
		processor.assignFromSimpleExpr(open.offset, bindings, operatorText, operatorIndex+1, end, constBindings)
		return end, true, nil
	}
	// Other right sides go into a temporary variable, so they run once. The
	// scan then continues into the right side and rewrites its gosling syntax.
	rightSideVar := processor.fresh("tmp")
	processor.result.TempNames = append(processor.result.TempNames, rightSideVar)
	tempOffset := processor.replace(open.offset, operator.end(), rightSideVar+" :=")
	if elementNames != nil {
		processor.result.ArrayTemps = append(processor.result.ArrayTemps, ArrayTemp{Offset: tempOffset, Names: elementNames})
	}
	processor.pendingDestructures = append(processor.pendingDestructures, &destructure{
		start:         processor.nesting(),
		text:          "; " + formatAssignment(bindings, operatorText, rightSideVar),
		newlines:      processor.collectNewlines(open.offset, operator.end()),
		constBindings: constBindings,
	})
	return operatorIndex, true, nil
}

// assignFromSimpleExpr rewrites a pattern whose right side is the simple
// expression from token rightSideStartIndex to token rightSideEndIndex. The
// names and the last copy of the right side stay source text, so the editor
// can map them.
func (processor *processor) assignFromSimpleExpr(patternStart int, bindings []binding, operatorText string, rightSideStartIndex, rightSideEndIndex int, constBindings []binding) {
	rightSideStart, rightSideEnd := processor.tokenAt(rightSideStartIndex).offset, processor.tokenAt(rightSideEndIndex).end()
	rightSide := string(processor.src[rightSideStart:rightSideEnd])
	// The output keeps the line count of a multi-line pattern.
	newlines := processor.collectNewlines(patternStart, rightSideEnd)
	if len(bindings) == 0 {
		assignText := formatAssignment(bindings, operatorText, rightSide)
		namesAt := processor.replace(patternStart, rightSideEnd, assignText+newlines)
		processor.recordNewlines(namesAt+len(assignText), newlines)
		return
	}
	namesAt := processor.copyNames(patternStart, bindings)
	lastBinding := bindings[len(bindings)-1]
	assignText := " " + operatorText + " "
	if leadingValues := values(bindings[:len(bindings)-1], rightSide); leadingValues != "" {
		assignText += leadingValues + ", "
	}
	processor.replace(lastBinding.offset+len(lastBinding.name), rightSideStart, assignText)
	pathAt := processor.replace(rightSideEnd, rightSideEnd, lastBinding.path+newlines)
	processor.recordNewlines(pathAt+len(lastBinding.path), newlines)
	processor.recordConsts(namesAt, constBindings)
}

// listFlatElementNames returns the names of an array pattern whose elements
// are all names, or nil for an object pattern, a nested pattern, or a rest element.
func listFlatElementNames(bindings []binding) []string {
	elementNames := make([]string, len(bindings))
	for bindingIndex, elementBinding := range bindings {
		if elementBinding.path != fmt.Sprintf("[%d]", bindingIndex) {
			return nil
		}
		elementNames[bindingIndex] = elementBinding.name
	}
	return elementNames
}

// pattern parses the bindings that start at token tokenIndex, after the
// opening bracket. prefix is the access path of the pattern. It returns the
// index after the closing bracket, or nil bindings if the tokens are not a
// pattern.
func (processor *processor) pattern(tokenIndex int, isArray bool, prefix string) ([]binding, int, error) {
	closing := token.RBRACE
	if isArray {
		closing = token.RBRACK
	}
	var bindings []binding
	for elementIndex := 0; ; elementIndex++ {
		current := processor.tokenAt(tokenIndex)
		if current.is(token.ELLIPSIS) {
			if !isArray {
				return nil, 0, &Error{current.offset, "rest elements are only supported in array destructuring"}
			}
			afterIndex, isClosed := processor.findPatternEnd(tokenIndex+2, closing)
			if !processor.tokenAt(tokenIndex+1).is(token.IDENT) || !isClosed {
				return nil, 0, nil
			}
			name := processor.tokenAt(tokenIndex + 1)
			bindings = append(bindings, binding{name: name.text, path: fmt.Sprintf("%s[%d:]", prefix, elementIndex), offset: name.offset})
			return bindings, afterIndex, nil
		}
		path := fmt.Sprintf("%s[%d]", prefix, elementIndex)
		if !isArray {
			if !current.is(token.IDENT) {
				return nil, 0, nil
			}
			path = prefix + "." + current.text
			if processor.tokenAt(tokenIndex + 1).is(token.COLON) {
				tokenIndex += 2
			}
		}
		elementBindings, nextIndex, err := processor.parseElement(tokenIndex, path)
		if err != nil || elementBindings == nil {
			return nil, 0, err
		}
		bindings = append(bindings, elementBindings...)
		tokenIndex = nextIndex
		if afterIndex, isClosed := processor.findPatternEnd(tokenIndex, closing); isClosed {
			return bindings, afterIndex, nil
		}
		if !processor.tokenAt(tokenIndex).is(token.COMMA) {
			return nil, 0, nil
		}
		tokenIndex++
	}
}

// parseElement parses one target at token tokenIndex: a name or a nested
// pattern. It returns the index after the target.
func (processor *processor) parseElement(tokenIndex int, path string) ([]binding, int, error) {
	current := processor.tokenAt(tokenIndex)
	switch {
	case current.is(token.IDENT):
		return []binding{{name: current.text, path: path, offset: current.offset}}, tokenIndex + 1, nil
	case current.is(token.LBRACE) || current.is(token.LBRACK):
		return processor.pattern(tokenIndex+1, current.is(token.LBRACK), path)
	}
	return nil, 0, nil
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
func formatAssignment(bindings []binding, operatorText, rightSide string) string {
	if len(bindings) == 0 {
		return "_ = " + rightSide
	}
	return names(bindings) + " " + operatorText + " " + values(bindings, rightSide)
}

func names(bindings []binding) string {
	targets := make([]string, len(bindings))
	for bindingIndex, binding := range bindings {
		targets[bindingIndex] = binding.name
	}
	return strings.Join(targets, ", ")
}

// copyNames replaces the pattern text from start up to the last name with
// the names, separated by commas. It returns the output offset of the first
// name.
func (processor *processor) copyNames(start int, bindings []binding) int {
	namesAt := processor.replace(start, bindings[0].offset, "")
	for bindingIndex := 1; bindingIndex < len(bindings); bindingIndex++ {
		previous := bindings[bindingIndex-1]
		processor.replace(previous.offset+len(previous.name), bindings[bindingIndex].offset, ", ")
	}
	return namesAt
}

func values(bindings []binding, rightSide string) string {
	sources := make([]string, len(bindings))
	for bindingIndex, binding := range bindings {
		sources[bindingIndex] = rightSide + binding.path
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
