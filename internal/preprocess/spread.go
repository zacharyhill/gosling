package preprocess

import (
	"go/token"
	"strings"
)

// Spread markers. [...a, x, ...b] becomes __goslingCat(a, __goslingOne(x), b).
// T{...a, k: v, ...b} becomes T(__goslingWith(a, __goslingKeys(T{k: v}), b)).
// {...a} without a type becomes __goslingWith(a), and &T{...a} becomes
// __goslingAddr(T(__goslingWith(a))). All keep the source text in order.
const (
	CatMarker  = "__goslingCat"
	OneMarker  = "__goslingOne"
	WithMarker = "__goslingWith"
	KeysMarker = "__goslingKeys"
	AddrMarker = "__goslingAddr"
)

type element struct {
	start, end int // token indexes, end is the comma or closing bracket
	spread     bool
}

// elements splits the bracketed list that opens at token openIndex into
// elements. It also returns the index of the closing token.
func (processor *processor) elements(openIndex int, closing token.Token) ([]element, int, error) {
	var elements []element
	depth, start := 0, openIndex+1
	for tokenIndex := openIndex + 1; tokenIndex < len(processor.tokens); tokenIndex++ {
		switch current := processor.tokens[tokenIndex]; current.kind {
		case token.LPAREN, token.LBRACK, token.LBRACE:
			depth++
		case token.RPAREN, token.RBRACK, token.RBRACE:
			if depth > 0 {
				depth--
				continue
			}
			if current.kind != closing {
				return nil, 0, &Error{current.offset, "unbalanced brackets"}
			}
			if start < tokenIndex {
				elements = append(elements, element{start, tokenIndex, processor.tokens[start].is(token.ELLIPSIS)})
			}
			return elements, tokenIndex, nil
		case token.COMMA:
			if depth == 0 {
				elements = append(elements, element{start, tokenIndex, processor.tokens[start].is(token.ELLIPSIS)})
				start = tokenIndex + 1
			}
		case token.SEMICOLON:
			if depth == 0 {
				return nil, 0, &Error{current.offset, "missing comma before newline in spread"}
			}
		}
	}
	return nil, 0, &Error{processor.tokens[openIndex].offset, "unbalanced brackets"}
}

func (processor *processor) insertBefore(tokenIndex int, text string) {
	processor.inserts[tokenIndex] += text
}

// sliceSpread rewrites the slice literal with spread elements that opens at
// hasSpreadElement reports whether an element of the list that opens at token
// openIndex starts with "...". The [...] of an array type is not a spread.
func (processor *processor) hasSpreadElement(openIndex int) bool {
	depth := 0
	for tokenIndex := openIndex + 1; tokenIndex < len(processor.tokens); tokenIndex++ {
		switch current := processor.tokens[tokenIndex]; current.kind {
		case token.LPAREN, token.LBRACK, token.LBRACE:
			depth++
		case token.RPAREN, token.RBRACK, token.RBRACE:
			if depth == 0 {
				return false
			}
			depth--
		case token.ELLIPSIS:
			previous := processor.tokens[tokenIndex-1]
			isElementStart := tokenIndex == openIndex+1 || previous.is(token.COMMA)
			if depth == 0 && isElementStart && !processor.tokenAt(tokenIndex+1).is(token.RBRACK) {
				return true
			}
		}
	}
	return false
}

// token tokenIndex.
func (processor *processor) sliceSpread(tokenIndex int) (int, error) {
	elements, closeIndex, err := processor.elements(tokenIndex, token.RBRACK)
	if err != nil {
		return 0, err
	}
	lbrack := processor.tokens[tokenIndex]
	processor.replace(lbrack.offset, lbrack.end(), CatMarker+"(")
	processor.parens++ // openBracket skips this [, but rewrite still counts its ]
	processor.markRuns(elements, OneMarker+"(", ")")
	processor.scheduled[closeIndex] = ")"
	return tokenIndex, nil
}

// markRuns drops the ... of each spread element and puts each run of other
// elements between runOpen and runClose.
func (processor *processor) markRuns(elements []element, runOpen, runClose string) {
	isInRun := false
	for elementIndex, current := range elements {
		if current.spread {
			processor.scheduled[current.start] = ""
			continue
		}
		if !isInRun {
			processor.insertBefore(current.start, runOpen)
			isInRun = true
		}
		if elementIndex == len(elements)-1 || elements[elementIndex+1].spread {
			processor.insertBefore(current.end, runClose)
			isInRun = false
		}
	}
}

// callSpread rewrites the spread at token ellipsisIndex if it is an argument
// of a call. f(a, ...xs) becomes f(a, xs...). Other spreads, as in
// f(...a, x, ...b), become f(__goslingCat(a, __goslingOne(x), b)...). It
// does nothing if the spread is not in a call.
func (processor *processor) callSpread(ellipsisIndex int) error {
	openIndex, ok := processor.findEnclosingBracket(ellipsisIndex)
	if !ok || !processor.isCall(openIndex) {
		return nil
	}
	elements, _, err := processor.elements(openIndex, token.RPAREN)
	if err != nil {
		return err
	}
	for len(elements) > 0 && !elements[0].spread {
		elements = elements[1:]
	}
	if len(elements) == 0 || elements[0].start != ellipsisIndex {
		return nil // the first spread of the call already rewrote this one
	}
	ellipsis := processor.tokens[ellipsisIndex]
	if len(elements) == 1 {
		processor.replace(ellipsis.offset, ellipsis.end(), "")
		processor.insertBefore(elements[0].end, "...")
		return nil
	}
	processor.replace(ellipsis.offset, ellipsis.end(), CatMarker+"(")
	processor.markRuns(elements[1:], OneMarker+"(", ")")
	processor.insertBefore(elements[len(elements)-1].end, ")...")
	return nil
}

// findEnclosingBracket returns the index of the innermost open bracket before
// token tokenIndex. It reports false if that bracket is not a (.
func (processor *processor) findEnclosingBracket(tokenIndex int) (int, bool) {
	depth := 0
	for openIndex := tokenIndex - 1; openIndex >= 0; openIndex-- {
		switch processor.tokens[openIndex].kind {
		case token.RPAREN, token.RBRACK, token.RBRACE:
			depth++
		case token.LPAREN, token.LBRACK, token.LBRACE:
			if depth == 0 {
				return openIndex, processor.tokens[openIndex].is(token.LPAREN)
			}
			depth--
		}
	}
	return 0, false
}

// isCall reports whether the ( at token openIndex starts the arguments of a
// call, and not the parameters of a function or method.
func (processor *processor) isCall(openIndex int) bool {
	if openIndex == 0 {
		return false
	}
	switch processor.tokens[openIndex-1].kind {
	case token.RPAREN, token.RBRACK, token.RBRACE:
		return true
	case token.IDENT:
		return !processor.isFuncDeclName(openIndex - 1)
	}
	return false
}

// isFuncDeclName reports whether the name at token nameIndex is the name of a
// function, a method, or an interface method.
func (processor *processor) isFuncDeclName(nameIndex int) bool {
	if nameIndex == 0 {
		return false
	}
	switch processor.tokens[nameIndex-1].kind {
	case token.FUNC:
		return true
	case token.RPAREN:
		receiverIndex, ok := processor.findEnclosingBracket(nameIndex - 1)
		return ok && receiverIndex > 0 && processor.tokens[receiverIndex-1].is(token.FUNC)
	case token.LBRACE, token.SEMICOLON:
		braceIndex, _ := processor.findEnclosingBracket(nameIndex)
		return braceIndex > 0 && processor.tokens[braceIndex].is(token.LBRACE) && processor.tokens[braceIndex-1].is(token.INTERFACE)
	}
	return false
}

// braceSpread rewrites T{...a, k: v, ...b} where the { is token tokenIndex.
// Each run of keyed elements becomes a KeysMarker call. Without a type
// before the {, the runs get the type UntypedMarker.
func (processor *processor) braceSpread(tokenIndex int) (int, error) {
	lbrace := processor.tokens[tokenIndex]
	elements, closeIndex, err := processor.elements(tokenIndex, token.RBRACE)
	if err != nil {
		return 0, err
	}
	afterClose := processor.tokenAt(closeIndex + 1)
	isDestructuring := processor.atStmtStart(tokenIndex) && (afterClose.is(token.ASSIGN) || afterClose.is(token.DEFINE))
	if isDestructuring {
		_, _, err := processor.destructure(tokenIndex, nil)
		return 0, err
	}
	runType, opening, closing := UntypedMarker, WithMarker+"(", ")"
	if typeStart, ok := processor.typeBefore(tokenIndex); ok {
		runType = strings.TrimSpace(string(processor.src[processor.tokens[typeStart].offset:lbrace.offset]))
		opening, closing = "("+opening, "))"
		addrClosing, err := processor.rewriteSpreadAddress(typeStart)
		if err != nil {
			return 0, err
		}
		closing += addrClosing
	}
	processor.replace(lbrace.offset, lbrace.end(), opening)
	processor.braceStack = append(processor.braceStack, frame{kind: frameBlock})
	processor.markRuns(elements, KeysMarker+"("+runType+"{", "})")
	processor.scheduled[closeIndex] = closing
	return tokenIndex, nil
}

// rewriteSpreadAddress rewrites the & before a spread whose type starts at
// token typeStart. It returns the text that closes the AddrMarker call.
func (processor *processor) rewriteSpreadAddress(typeStart int) (string, error) {
	if typeStart == 0 || !processor.tokens[typeStart-1].is(token.AND) {
		return "", nil
	}
	ampersand := processor.tokens[typeStart-1]
	if ampersand.offset < processor.copiedUpTo {
		return "", &Error{ampersand.offset, "cannot take the address of this spread"}
	}
	processor.replace(ampersand.offset, ampersand.end(), AddrMarker+"(")
	return ")", nil
}

// typeBefore finds the start of a type, like T, pkg.T, T[int], []T, or
// map[K]*V, that ends directly before token tokenIndex.
func (processor *processor) typeBefore(tokenIndex int) (int, bool) {
	typeStart := tokenIndex - 1
	if typeStart >= 0 && processor.tokens[typeStart].is(token.RBRACK) {
		typeStart = processor.findOpeningBracket(typeStart) - 1
	}
	if typeStart < 0 || !processor.tokens[typeStart].is(token.IDENT) {
		return 0, false
	}
	if typeStart >= 2 && processor.tokens[typeStart-1].is(token.PERIOD) && processor.tokens[typeStart-2].is(token.IDENT) {
		typeStart -= 2
	}
	return processor.extendTypePrefixes(typeStart), true
}

// extendTypePrefixes extends a type that starts at token typeStart over the []
// and map[K] prefixes before it. A * counts only inside such a prefix.
func (processor *processor) extendTypePrefixes(typeStart int) int {
	confirmed := typeStart
	for typeStart > 0 {
		previous := processor.tokens[typeStart-1]
		if previous.is(token.MUL) {
			typeStart--
			continue
		}
		if !previous.is(token.RBRACK) {
			break
		}
		lbrack := processor.findOpeningBracket(typeStart - 1)
		switch {
		case lbrack == typeStart-2:
			typeStart = lbrack
		case lbrack > 0 && processor.tokens[lbrack-1].is(token.MAP):
			typeStart = lbrack - 1
		default:
			return confirmed
		}
		confirmed = typeStart
	}
	return confirmed
}

// findOpeningBracket returns the index of the [ that the ] at token closeIndex
// closes, or -1.
func (processor *processor) findOpeningBracket(closeIndex int) int {
	depth := 0
	for tokenIndex := closeIndex; tokenIndex >= 0; tokenIndex-- {
		switch processor.tokens[tokenIndex].kind {
		case token.RBRACK:
			depth++
		case token.LBRACK:
			depth--
			if depth == 0 {
				return tokenIndex
			}
		}
	}
	return -1
}

// findSpreadBraces returns the indexes of the { tokens with a spread element.
func findSpreadBraces(tokens []lexeme) map[int]bool {
	braces := map[int]bool{}
	var openers []int
	for tokenIndex, current := range tokens {
		switch current.kind {
		case token.LPAREN, token.LBRACK, token.LBRACE:
			openers = append(openers, tokenIndex)
		case token.RPAREN, token.RBRACK, token.RBRACE:
			if len(openers) > 0 {
				openers = openers[:len(openers)-1]
			}
		case token.ELLIPSIS:
			if len(openers) == 0 {
				continue
			}
			opener := openers[len(openers)-1]
			if tokens[opener].is(token.LBRACE) && (opener == tokenIndex-1 || tokens[tokenIndex-1].is(token.COMMA)) {
				braces[opener] = true
			}
		}
	}
	return braces
}
