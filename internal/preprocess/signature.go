package preprocess

import "go/token"

const (
	everyParamMessage     = "write name: type for every parameter, or use Go order for all of them"
	resultColonMessage    = "write ): before the result type of a function with name: type parameters"
	genericUntypedMessage = "the parameters of a generic arrow function need types"
)

// paramList is a parenthesized parameter list. close is the token index of
// the ")". colons holds the index of the ":" in each annotated entry.
// plainEntries holds the first token of each entry without one.
type paramList struct {
	close        int
	colons       []int
	plainEntries []int
}

func (list paramList) isAnnotated() bool { return len(list.colons) > 0 }

// signature removes the ":" of TypeScript-style annotations in the func
// signature at funcIndex:
//
//	func f(a: int, b: string): bool  ->  func f(a int, b string) bool
//
// A signature uses one order. The receiver is not part of this rule.
func (processor *processor) signature(funcIndex int) error {
	tokenIndex := funcIndex + 1
	if processor.tokenAt(tokenIndex).is(token.LPAREN) {
		firstList, ok := processor.parseParamList(tokenIndex)
		if !ok {
			return nil
		}
		tokenIndex = firstList.close + 1
		if !processor.tokenAt(tokenIndex).is(token.IDENT) {
			return processor.signatureFrom(firstList, processor.inCaseClause(funcIndex))
		}
		if err := processor.stripColons(firstList, firstList.isAnnotated()); err != nil {
			return err
		}
	}
	if !processor.tokenAt(tokenIndex).is(token.IDENT) {
		return nil
	}
	tokenIndex++
	if processor.tokenAt(tokenIndex).is(token.LBRACK) {
		closeIndex, ok := processor.matchBracket(tokenIndex)
		if !ok {
			return nil
		}
		tokenIndex = closeIndex + 1
	}
	params, ok := processor.parseParamList(tokenIndex)
	if !ok {
		return nil
	}
	return processor.signatureFrom(params, false)
}

// interfaceMethods handles the signatures of the methods in the interface
// type at interfaceIndex.
func (processor *processor) interfaceMethods(interfaceIndex int) error {
	braceIndex := interfaceIndex + 1
	closeIndex, ok := processor.matchClose(braceIndex)
	if !ok {
		return nil
	}
	for tokenIndex := braceIndex + 1; tokenIndex < closeIndex; tokenIndex++ {
		current, previous := processor.tokens[tokenIndex], processor.tokens[tokenIndex-1]
		isEntryStart := previous.is(token.LBRACE) || previous.is(token.SEMICOLON)
		if !isEntryStart || !current.is(token.IDENT) || !processor.tokenAt(tokenIndex+1).is(token.LPAREN) {
			continue
		}
		params, ok := processor.parseParamList(tokenIndex + 1)
		if !ok {
			return nil
		}
		if err := processor.signatureFrom(params, false); err != nil {
			return err
		}
		tokenIndex = params.close
	}
	return nil
}

// signatureFrom checks the parameters and results that start with params
// and schedules the removal of their annotation colons. In a case clause, a
// ":" after the parameters ends the case, so it is not a result colon.
func (processor *processor) signatureFrom(params paramList, isInCase bool) error {
	resultIndex := params.close + 1
	hasResultColon := !isInCase && processor.tokenAt(resultIndex).is(token.COLON)
	if hasResultColon {
		resultIndex++
	}
	results, hasResultList := processor.parseParamList(resultIndex)
	isAnnotated := params.isAnnotated() || hasResultColon || (hasResultList && results.isAnnotated())
	if err := processor.stripColons(params, isAnnotated); err != nil {
		return err
	}
	if hasResultList {
		if err := processor.stripColons(results, results.isAnnotated()); err != nil {
			return err
		}
	}
	if isAnnotated && !hasResultColon && isTypeStart(processor.tokenAt(resultIndex)) {
		return &Error{processor.tokenAt(resultIndex).offset, resultColonMessage}
	}
	if hasResultColon {
		processor.scheduled[params.close+1] = ""
	}
	return nil
}

// stripColons schedules the removal of the colons in list. If isAnnotated
// is true, every entry must have a colon.
func (processor *processor) stripColons(list paramList, isAnnotated bool) error {
	if isAnnotated && len(list.plainEntries) > 0 {
		return &Error{processor.tokens[list.plainEntries[0]].offset, everyParamMessage}
	}
	for _, colonIndex := range list.colons {
		processor.scheduled[colonIndex] = ""
	}
	return nil
}

// parseParamList parses the list that opens at openIndex. Each entry is either
// `name: type` or plain Go.
func (processor *processor) parseParamList(openIndex int) (paramList, bool) {
	if !processor.tokenAt(openIndex).is(token.LPAREN) {
		return paramList{}, false
	}
	closeIndex, ok := processor.matchClose(openIndex)
	if !ok {
		return paramList{}, false
	}
	list := paramList{close: closeIndex}
	entryStart, depth := openIndex+1, 0
	for tokenIndex := openIndex + 1; tokenIndex <= closeIndex; tokenIndex++ {
		switch processor.tokens[tokenIndex].kind {
		case token.LPAREN, token.LBRACK, token.LBRACE:
			depth++
			continue
		case token.RPAREN, token.RBRACK, token.RBRACE:
			if depth > 0 {
				depth--
				continue
			}
		case token.COMMA, token.SEMICOLON:
			if depth > 0 {
				continue
			}
		default:
			continue
		}
		if tokenIndex > entryStart {
			switch {
			case processor.isTypedPatternEntry(entryStart):
				// A destructured parameter fits both orders. Its rename removes its colon.
			case processor.tokens[entryStart].is(token.IDENT) && processor.tokens[entryStart+1].is(token.COLON):
				list.colons = append(list.colons, entryStart+1)
			default:
				list.plainEntries = append(list.plainEntries, entryStart)
			}
		}
		entryStart = tokenIndex + 1
	}
	return list, true
}

// isTypedPatternEntry reports whether the parameter at token entryStart is a
// destructuring pattern with a type, like {a, b}: T.
func (processor *processor) isTypedPatternEntry(entryStart int) bool {
	open := processor.tokens[entryStart]
	if !open.is(token.LBRACE) && !open.is(token.LBRACK) {
		return false
	}
	closeIndex, ok := processor.matchClose(entryStart)
	return ok && processor.tokenAt(closeIndex+1).is(token.COLON)
}

// typedArrow inserts "func" before an arrow parameter list with types,
// destructured parameters, or a result type, like (x: int) => x, ({a}) => a,
// or (x): string => f(x). It runs at the "(", before the scan copies the
// list, so rewrites inside the list do not block the insert.
func (processor *processor) typedArrow(openIndex int) error {
	list, ok := processor.parseParamList(openIndex)
	if !ok || (openIndex > 0 && processor.isArrowResultColon(openIndex-1)) {
		return nil
	}
	arrowIndex, hasResult := processor.findArrowAfterResult(list.close + 1)
	openParen := processor.tokens[openIndex]
	if hasResult && openParen.offset < processor.copiedUpTo {
		return &Error{openParen.offset, "unsupported arrow function"}
	}
	if !hasResult && (!processor.isArrow(list.close+1) || openParen.offset < processor.copiedUpTo) {
		return nil
	}
	isGeneric := processor.genericParamOffsets[openParen.offset]
	if _, isUntyped := processor.untypedParams(openIndex+1, list.close); isUntyped && !hasResult {
		if isGeneric {
			return &Error{openParen.offset, genericUntypedMessage}
		}
		return nil
	}
	patterns, err := processor.findParamPatterns(openIndex, list.close)
	if err != nil {
		return err
	}
	isUntyped := processor.isUntypedParamList(openIndex+1, list.close)
	if isUntyped && isGeneric {
		return &Error{openParen.offset, genericUntypedMessage}
	}
	if err := processor.stripArrowColons(list, isUntyped, arrowIndex, hasResult); err != nil {
		return err
	}
	if !isGeneric {
		// A generic arrow already has its func keyword.
		processor.replace(openParen.offset, openParen.offset, "func")
	}
	return processor.registerArrowPatterns(openIndex, list.close, patterns, isUntyped)
}

// stripArrowColons removes the annotation colons of an arrow parameter list,
// and the colons of its result type if hasResult is true.
func (processor *processor) stripArrowColons(list paramList, isUntyped bool, arrowIndex int, hasResult bool) error {
	if !isUntyped {
		// With a result type, the parameters must use name: type.
		if err := processor.stripColons(list, hasResult || list.isAnnotated()); err != nil {
			return err
		}
	}
	if !hasResult {
		return nil
	}
	return processor.prepareArrowResult(list.close, arrowIndex)
}

// prepareArrowResult removes the colon before the result type of an arrow
// whose parameters close at token closeIndex, and the colons of named
// results. The "=>" at token arrowIndex then needs no result placeholder.
func (processor *processor) prepareArrowResult(closeIndex, arrowIndex int) error {
	colonIndex := closeIndex + 1
	if results, ok := processor.parseParamList(colonIndex + 1); ok {
		if err := processor.stripColons(results, results.isAnnotated()); err != nil {
			return err
		}
	}
	processor.scheduled[colonIndex] = ""
	processor.resultArrows[processor.tokens[arrowIndex].offset] = processor.tokens[closeIndex].offset
	return nil
}

// registerArrowPatterns marks the arrow parameter list from openIndex to
// closeIndex as handled and schedules the renames of its patterns.
func (processor *processor) registerArrowPatterns(openIndex, closeIndex int, patterns []paramPattern, isUntyped bool) error {
	closeOffset := processor.tokens[closeIndex].offset
	processor.typedArrows[closeOffset] = true
	statements, err := processor.registerPatternRenames(patterns, isUntyped)
	if err != nil {
		return err
	}
	if isUntyped {
		processor.markUntypedNames(openIndex+1, closeIndex)
	}
	processor.arrowStatements[closeOffset] = statements
	return nil
}

func (processor *processor) isArrow(tokenIndex int) bool {
	return processor.tokenAt(tokenIndex).is(token.ASSIGN) && processor.tokenAt(tokenIndex+1).is(token.GTR) && processor.touchesNext(tokenIndex)
}

// inCaseClause reports whether the token at tokenIndex is in the expression
// list of a case clause.
func (processor *processor) inCaseClause(tokenIndex int) bool {
	depth := 0
	for tokenIndex--; tokenIndex >= 0; tokenIndex-- {
		switch processor.tokens[tokenIndex].kind {
		case token.RPAREN, token.RBRACK, token.RBRACE:
			depth++
		case token.LPAREN, token.LBRACK:
			if depth > 0 {
				depth--
			}
		case token.LBRACE:
			if depth == 0 {
				return false
			}
			depth--
		case token.SEMICOLON, token.COLON:
			if depth == 0 {
				return false
			}
		case token.CASE:
			if depth == 0 {
				return true
			}
		}
	}
	return false
}

// isTypeStart reports whether a type can start with current.
func isTypeStart(current lexeme) bool {
	switch current.kind {
	case token.IDENT, token.MUL, token.LBRACK, token.LPAREN, token.MAP, token.CHAN, token.ARROW, token.FUNC, token.STRUCT, token.INTERFACE:
		return true
	}
	return false
}

// matchClose returns the index of the bracket that closes the one at
// openIndex.
func (processor *processor) matchClose(openIndex int) (int, bool) {
	depth := 0
	for tokenIndex := openIndex; tokenIndex < len(processor.tokens); tokenIndex++ {
		switch processor.tokens[tokenIndex].kind {
		case token.LPAREN, token.LBRACK, token.LBRACE:
			depth++
		case token.RPAREN, token.RBRACK, token.RBRACE:
			depth--
			if depth == 0 {
				return tokenIndex, true
			}
		}
	}
	return 0, false
}
