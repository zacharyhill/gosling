package preprocess

import "go/token"

// forOf rewrites `for (const x of xs) {` into `for _, x := range xs {` and
// `for (const k in m) {` into `for k := range m {`. It does nothing if the
// tokens at tokenIndex do not match.
func (processor *processor) forOf(tokenIndex int) {
	lparen, declaration, name, keyword := processor.tokenAt(tokenIndex+1), processor.tokenAt(tokenIndex+2), processor.tokenAt(tokenIndex+3), processor.tokenAt(tokenIndex+4)
	if !(declaration.is(token.CONST) || declaration.is(token.VAR) || declaration.isIdent("let")) ||
		!name.is(token.IDENT) || !(keyword.isIdent("of") || keyword.isIdent("in")) {
		processor.cStyleFor(tokenIndex)
		return
	}
	rparenIndex, hasBrace := processor.matchParen(tokenIndex + 1)
	if rparenIndex < 0 || !processor.tokenAt(rparenIndex+1).is(token.LBRACE) {
		return
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
