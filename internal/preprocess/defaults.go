package preprocess

import (
	"go/token"
	"strings"
)

// Default records a parameter with a default value in a function
// declaration. The value is the text [ValueStart, ValueEnd) of the original
// source. An optional parameter has no value.
type Default struct {
	Param                int // original offset of the parameter name
	ValueStart, ValueEnd int
	Optional             bool
}

// findDefaults finds the default values and optional markers in the parameters
// of the function declaration at token funcIndex. It records them, and it
// marks their tokens for removal. A malformed declaration is left to the Go
// parser.
func (processor *processor) findDefaults(funcIndex int) error {
	paramsIndex := funcIndex + 1
	if processor.tokenAt(paramsIndex).is(token.LPAREN) {
		_, receiverEnd, err := processor.elements(paramsIndex, token.RPAREN)
		if err != nil {
			return nil
		}
		paramsIndex = receiverEnd + 1
	}
	if !processor.tokenAt(paramsIndex).is(token.IDENT) {
		return nil
	}
	paramsIndex++
	if processor.tokenAt(paramsIndex).is(token.LBRACK) {
		typeParamsEnd, ok := processor.matchBracket(paramsIndex)
		if !ok {
			return nil
		}
		paramsIndex = typeParamsEnd + 1
	}
	if !processor.tokenAt(paramsIndex).is(token.LPAREN) {
		return nil
	}
	params, _, err := processor.elements(paramsIndex, token.RPAREN)
	if err != nil {
		return nil
	}
	return processor.markDefaults(params)
}

// markDefaults checks the parameters with default values. They must come
// last, and each one needs its own name and type.
func (processor *processor) markDefaults(params []element) error {
	hasSeenDefault := false
	for paramIndex, param := range params {
		name := processor.tokens[param.start]
		assignIndex := processor.findTopLevelAssign(param.start, param.end)
		isOptional := param.end-param.start > 2 && processor.tokens[param.start+1].is(token.ILLEGAL) && processor.tokens[param.start+1].text == "?"
		if assignIndex < 0 && !isOptional {
			if hasSeenDefault {
				return &Error{name.offset, "a parameter without a default value cannot follow one with a default value"}
			}
			continue
		}
		hasSeenDefault = true
		if err := processor.checkDefaultParam(params, paramIndex, assignIndex, isOptional); err != nil {
			return err
		}
		if isOptional {
			processor.removals[param.start+1] = param.start + 1
			processor.result.Defaults = append(processor.result.Defaults, Default{Param: name.offset, Optional: true})
			continue
		}
		// Calls get a copy of the source text of the value.
		if offset := processor.findGoslingSyntax(assignIndex+1, param.end); offset >= 0 {
			return &Error{offset, "a default value must be plain Go; use a constant or a function"}
		}
		processor.removals[assignIndex] = param.end - 1
		processor.result.Defaults = append(processor.result.Defaults, Default{
			Param:      name.offset,
			ValueStart: processor.tokens[assignIndex+1].offset,
			ValueEnd:   processor.tokens[param.end-1].end(),
		})
	}
	return nil
}

// checkDefaultParam checks the parameter with a default value or an
// optional marker at paramIndex.
func (processor *processor) checkDefaultParam(params []element, paramIndex, assignIndex int, isOptional bool) error {
	param := params[paramIndex]
	name := processor.tokens[param.start]
	typeStart, typeEnd := param.start+1, param.end
	if isOptional {
		typeStart++
	}
	if assignIndex >= 0 {
		typeEnd = assignIndex
	}
	switch {
	case !name.is(token.IDENT) || typeStart >= typeEnd:
		return &Error{name.offset, "a parameter with a default value needs a name and a type"}
	case paramIndex > 0 && params[paramIndex-1].end-params[paramIndex-1].start == 1:
		return &Error{name.offset, "a parameter with a default value cannot share its type with other parameters"}
	case processor.tokens[typeStart].is(token.ELLIPSIS):
		return &Error{name.offset, "a variadic parameter cannot have a default value"}
	case isOptional && assignIndex >= 0:
		return &Error{name.offset, "an optional parameter cannot also have a default value"}
	case assignIndex == param.end-1:
		return &Error{processor.tokens[assignIndex].offset, "missing default value"}
	}
	return nil
}

// findTopLevelAssign returns the index of the first "=" in tokens [from, to)
// outside brackets, or -1.
func (processor *processor) findTopLevelAssign(from, to int) int {
	depth := 0
	for tokenIndex := from; tokenIndex < to; tokenIndex++ {
		switch processor.tokens[tokenIndex].kind {
		case token.LPAREN, token.LBRACK, token.LBRACE:
			depth++
		case token.RPAREN, token.RBRACK, token.RBRACE:
			depth--
		case token.ASSIGN:
			if depth == 0 {
				return tokenIndex
			}
		}
	}
	return -1
}

// removeMarked removes tokens [tokenIndex, lastIndex] and the space before
// them. It keeps their newlines, so line numbers do not change.
func (processor *processor) removeMarked(tokenIndex, lastIndex int) int {
	start := processor.tokens[tokenIndex].offset
	if tokenIndex > 0 {
		start = max(processor.tokens[tokenIndex-1].end(), processor.copiedUpTo)
	}
	end := processor.tokens[lastIndex].end()
	processor.replace(start, end, strings.Repeat("\n", strings.Count(string(processor.src[start:end]), "\n")))
	return lastIndex
}
