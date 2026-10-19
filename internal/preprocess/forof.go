package preprocess

import "go/token"

// forOf rewrites `for (const x of xs) {` into `for _, x := range xs {` and
// `for (const k in m) {` into `for k := range m {`. It does nothing if the
// tokens at tokenIndex do not match. It returns the index of the last token
// it uses.
func (processor *processor) forOf(tokenIndex int) (int, error) {
	lparen, declaration, name, keyword := processor.tokenAt(tokenIndex+1), processor.tokenAt(tokenIndex+2), processor.tokenAt(tokenIndex+3), processor.tokenAt(tokenIndex+4)
	isDeclaration := declaration.is(token.CONST) || declaration.is(token.VAR) || declaration.isIdent("let")
	if isDeclaration && (name.is(token.LBRACE) || name.is(token.LBRACK)) {
		return processor.forOfPattern(tokenIndex)
	}
	if !isDeclaration || !name.is(token.IDENT) || !(keyword.isIdent("of") || keyword.isIdent("in")) {
		processor.cStyleFor(tokenIndex)
		return tokenIndex, nil
	}
	rparenIndex, hasBrace := processor.matchParen(tokenIndex + 1)
	if rparenIndex < 0 || !processor.tokenAt(rparenIndex+1).is(token.LBRACE) {
		return tokenIndex, nil
	}
	variables := name.text
	if keyword.text == "of" {
		variables = "_, " + name.text
	}
	leadingSpace := processor.getSpaceAfterFor(tokenIndex)
	text := leadingSpace + variables + " := range "
	// Go requires parentheses around a composite literal in a for header.
	if hasBrace {
		text += "("
	} else {
		processor.scheduled[rparenIndex] = ""
	}
	textStart := processor.replace(lparen.offset, processor.tokenAt(tokenIndex+5).offset, text)
	if keyword.text == "of" {
		processor.result.ForOfBlanks = append(processor.result.ForOfBlanks, textStart+len(leadingSpace))
	}
	if declaration.is(token.CONST) {
		processor.result.Consts = append(processor.result.Consts, textStart+len(leadingSpace)+len(variables)-len(name.text))
	}
	return tokenIndex, nil
}

// forOfPattern rewrites `for (const {a, b} of xs) {` into
// `for _, tmp := range xs { a, b := tmp.a, tmp.b;`. It does nothing if the
// tokens at tokenIndex do not match. It returns the index of the last token
// it uses.
func (processor *processor) forOfPattern(tokenIndex int) (int, error) {
	lparen, declaration, patternOpen := processor.tokenAt(tokenIndex+1), processor.tokenAt(tokenIndex+2), processor.tokenAt(tokenIndex+3)
	isArray := patternOpen.is(token.LBRACK)
	bindings, keywordIndex, err := processor.pattern(tokenIndex+4, isArray, "")
	if err != nil || bindings == nil {
		return tokenIndex, err
	}
	if err := checkDefaults(bindings); err != nil {
		return tokenIndex, err
	}
	bindings = removeBlankElements(bindings)
	keyword := processor.tokenAt(keywordIndex)
	if keyword.isIdent("in") {
		return tokenIndex, &Error{patternOpen.offset, "destructuring in a loop needs for...of"}
	}
	rparenIndex, _ := processor.matchParen(tokenIndex + 1)
	if !keyword.isIdent("of") || rparenIndex < 0 || !processor.tokenAt(rparenIndex+1).is(token.LBRACE) {
		return tokenIndex, nil
	}
	elementVar := processor.fresh("tmp")
	processor.result.TempNames = append(processor.result.TempNames, elementVar)
	leadingSpace := processor.getSpaceAfterFor(tokenIndex)
	rangeClause := leadingSpace + "_, " + elementVar + " := range "
	if processor.hasBrace(keywordIndex+1, rparenIndex) {
		rangeClause += "("
	} else {
		processor.scheduled[rparenIndex] = ""
	}
	clauseStart := processor.replace(lparen.offset, processor.tokenAt(keywordIndex+1).offset, rangeClause)
	processor.result.ForOfBlanks = append(processor.result.ForOfBlanks, clauseStart+len(leadingSpace))
	statement := &destructure{text: " " + formatAssignment(bindings, ":=", elementVar) + ";"}
	if declaration.is(token.CONST) {
		statement.constBindings = bindings
	}
	processor.bodyDestructures[rparenIndex+1] = statement
	// The scan skips the pattern, so it counts the ( here. The ) stays in
	// the scan.
	processor.parens++
	return keywordIndex, nil
}

// hasBrace reports whether a { is in tokens [from, to).
func (processor *processor) hasBrace(from, to int) bool {
	for tokenIndex := from; tokenIndex < to; tokenIndex++ {
		if processor.tokens[tokenIndex].is(token.LBRACE) {
			return true
		}
	}
	return false
}

// cStyleFor removes the parentheses from `for (init; cond; post) {`. A
// declaration in init is rewritten when the scan reaches it.
func (processor *processor) cStyleFor(tokenIndex int) {
	rparenIndex, _ := processor.matchParen(tokenIndex + 1)
	if rparenIndex < 0 || !processor.tokenAt(rparenIndex+1).is(token.LBRACE) {
		return
	}
	depth, semicolons := 0, 0
	for _, current := range processor.tokens[tokenIndex+2 : rparenIndex] {
		switch {
		case current.is(token.LPAREN) || current.is(token.LBRACK) || current.is(token.LBRACE):
			depth++
		case current.is(token.RPAREN) || current.is(token.RBRACK) || current.is(token.RBRACE):
			depth--
		case current.is(token.SEMICOLON) && depth == 0:
			semicolons++
		}
	}
	if semicolons != 2 {
		return
	}
	lparen := processor.tokens[tokenIndex+1]
	processor.replace(lparen.offset, lparen.end(), processor.getSpaceAfterFor(tokenIndex))
	processor.scheduled[rparenIndex] = ""
}

// matchParen returns the index of the ) that matches the ( at token
// lparenIndex. It also reports whether a { is between them.
func (processor *processor) matchParen(lparenIndex int) (int, bool) {
	depth, hasBrace := 0, false
	for tokenIndex := lparenIndex; tokenIndex < len(processor.tokens); tokenIndex++ {
		switch processor.tokens[tokenIndex].kind {
		case token.LPAREN:
			depth++
		case token.RPAREN:
			depth--
			if depth == 0 {
				return tokenIndex, hasBrace
			}
		case token.LBRACE:
			hasBrace = true
		}
	}
	return -1, false
}

// getSpaceAfterFor returns the text that replaces the ( after the for at
// tokenIndex. In "for(", a space keeps the next text apart from the keyword.
func (processor *processor) getSpaceAfterFor(tokenIndex int) string {
	if processor.touchesNext(tokenIndex) {
		return " "
	}
	return ""
}
