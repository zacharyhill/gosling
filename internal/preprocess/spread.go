package preprocess

import (
	"go/token"
	"strings"
)

// Spread markers. [...a, x, ...b] becomes __goslingCat(a, __goslingOne(x), b).
// T{...p, X: 1} becomes T(__goslingWith(p, T{X: 1})), and &T{...p} becomes
// __goslingAddr(T(__goslingWith(p, T{}))). All keep the source text in order.
const (
	CatMarker  = "__goslingCat"
	OneMarker  = "__goslingOne"
	WithMarker = "__goslingWith"
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

func (processor *processor) insertBefore(tokenIndex int, text string) {
	processor.inserts[tokenIndex] += text
}

// sliceSpread rewrites the slice literal with spread elements that opens at
// token tokenIndex.
func (processor *processor) sliceSpread(tokenIndex int) (int, error) {
	elements, closeIndex, err := processor.elements(tokenIndex, token.RBRACK)
	if err != nil {
		return 0, err
	}
	lbrack := processor.tokens[tokenIndex]
	processor.replace(lbrack.offset, lbrack.end(), CatMarker+"(")
	processor.parens++ // openBracket skips this [, but rewrite still counts its ]
	inRun := false
	for elementIndex, current := range elements {
		if current.spread {
			processor.scheduled[current.start] = ""
			continue
		}
		if !inRun {
			processor.insertBefore(current.start, OneMarker+"(")
			inRun = true
		}
		if elementIndex == len(elements)-1 || elements[elementIndex+1].spread {
			processor.insertBefore(current.end, ")")
			inRun = false
		}
	}
	processor.scheduled[closeIndex] = ")"
	return tokenIndex, nil
}

// structSpread rewrites T{...p, fields} where the { is token tokenIndex.
func (processor *processor) structSpread(tokenIndex int) (int, error) {
	lbrace := processor.tokens[tokenIndex]
	typeStart, ok := processor.typeBefore(tokenIndex)
	if !ok {
		return 0, &Error{lbrace.offset, "struct spread needs a type name before {"}
	}
	typeText := strings.TrimSpace(string(processor.src[processor.tokens[typeStart].offset:lbrace.offset]))
	elements, closeIndex, err := processor.elements(tokenIndex, token.RBRACE)
	if err != nil {
		return 0, err
	}
	for _, field := range elements[1:] {
		if field.spread {
			return 0, &Error{processor.tokens[field.start].offset, "struct spread must be the first element"}
		}
	}
	addrClosing := ""
	if typeStart > 0 && processor.tokens[typeStart-1].is(token.AND) {
		ampersand := processor.tokens[typeStart-1]
		if ampersand.offset < processor.copiedUpTo {
			return 0, &Error{ampersand.offset, "cannot take the address of this struct spread"}
		}
		processor.replace(ampersand.offset, ampersand.end(), AddrMarker+"(")
		addrClosing = ")"
	}
	ellipsis := processor.tokenAt(tokenIndex + 1)
	processor.replace(lbrace.offset, lbrace.end(), "("+WithMarker+"(")
	processor.replace(ellipsis.offset, ellipsis.end(), "")
	processor.braceStack = append(processor.braceStack, frame{kind: frameBlock})
	hasTrailingComma := elements[0].end < closeIndex
	switch {
	case len(elements) > 1:
		processor.scheduled[elements[0].end] = ", " + typeText + "{"
		processor.scheduled[closeIndex] = "}))" + addrClosing
	case hasTrailingComma:
		processor.scheduled[closeIndex] = typeText + "{}))" + addrClosing
	default:
		processor.scheduled[closeIndex] = ", " + typeText + "{}))" + addrClosing
	}
	return tokenIndex + 1, nil
}

// typeBefore finds the start of a type name, like T, pkg.T, or T[int], that
// ends directly before token tokenIndex.
func (processor *processor) typeBefore(tokenIndex int) (int, bool) {
	typeStart := tokenIndex - 1
	if typeStart >= 0 && processor.tokens[typeStart].is(token.RBRACK) {
		depth := 0
		for ; typeStart >= 0; typeStart-- {
			switch processor.tokens[typeStart].kind {
			case token.RBRACK:
				depth++
			case token.LBRACK:
				depth--
			}
			if depth == 0 {
				break
			}
		}
		typeStart--
	}
	if typeStart < 0 || !processor.tokens[typeStart].is(token.IDENT) {
		return 0, false
	}
	if typeStart >= 2 && processor.tokens[typeStart-1].is(token.PERIOD) && processor.tokens[typeStart-2].is(token.IDENT) {
		typeStart -= 2
	}
	return typeStart, true
}

// spreadInBraces reports whether the ... at token ellipsisIndex directly follows a
// comma inside braces, as in T{X: 1, ...p}.
func (processor *processor) spreadInBraces(ellipsisIndex int) bool {
	if ellipsisIndex == 0 || !processor.tokens[ellipsisIndex-1].is(token.COMMA) {
		return false
	}
	depth := 0
	for tokenIndex := ellipsisIndex - 1; tokenIndex >= 0; tokenIndex-- {
		switch processor.tokens[tokenIndex].kind {
		case token.RPAREN, token.RBRACK, token.RBRACE:
			depth++
		case token.LPAREN, token.LBRACK:
			if depth == 0 {
				return false
			}
			depth--
		case token.LBRACE:
			if depth == 0 {
				return true
			}
			depth--
		}
	}
	return false
}
