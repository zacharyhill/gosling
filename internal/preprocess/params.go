package preprocess

import (
	"go/token"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A destructured parameter gets a generated name. gosling declares the
// pattern names at the start of the body:
//
//	func handle({Name, Port}: Config) {
//	func handle(config Config) { Name, Port := config.Name, config.Port;

// defaultParamName names a destructured parameter whose type has no
// exported name.
const defaultParamName = "arg"

// paramPattern is a destructuring pattern in a parameter list.
type paramPattern struct {
	openIndex int
	open      lexeme
	isArray   bool
	bindings  []binding
	// typeStart is the index of the first token of the type, or -1 if the
	// parameter has no type.
	typeStart int
	name      string
}

// paramStatements holds the statements that declare the names of
// destructured parameters. starts holds the offset of each statement in text.
type paramStatements struct {
	text   string
	starts []int
}

// rewriteFuncHeader rewrites the header of the function whose func keyword
// is at token tokenIndex. It returns the index of the last token it uses.
func (processor *processor) rewriteFuncHeader(tokenIndex int) (int, error) {
	isFuncDecl := len(processor.braceStack) == 0 && processor.parens == 0 && processor.atStmtStart(tokenIndex)
	headerIndex, lbraceIndex := tokenIndex, -1
	if processor.isGeneratorKeyword(tokenIndex) {
		var err error
		if lbraceIndex, err = processor.generator(tokenIndex); err != nil {
			return 0, err
		}
		// The rest of the header follows the *, as it follows func.
		headerIndex = tokenIndex + 1
	}
	if processor.currentImpl != nil && processor.currentImpl.funcKeywordIndexes[tokenIndex] {
		processor.addImplReceiver(processor.tokens[headerIndex])
	}
	if isFuncDecl {
		if err := processor.findDefaults(headerIndex); err != nil {
			return 0, err
		}
	}
	if err := processor.signature(headerIndex); err != nil {
		return 0, err
	}
	lastIndex, err := processor.rewriteFuncParams(headerIndex)
	if lbraceIndex >= 0 {
		processor.moveParamStatements(lbraceIndex)
	}
	return lastIndex, err
}

// rewriteFuncParams rewrites the destructured parameters of the function whose
// func keyword is at token tokenIndex. It returns the index of the last
// token it uses.
func (processor *processor) rewriteFuncParams(tokenIndex int) (int, error) {
	lparenIndex, ok := processor.findParamListStart(tokenIndex)
	if !ok {
		return tokenIndex, nil
	}
	rparenIndex := processor.findClose(lparenIndex)
	patterns, err := processor.findParamPatterns(lparenIndex, rparenIndex)
	if err != nil || len(patterns) == 0 {
		return tokenIndex, err
	}
	bodyIndex, hasBody := processor.findBodyStart(rparenIndex)
	if !hasBody {
		return 0, &Error{patterns[0].open.offset, "a destructured parameter needs a function body"}
	}
	statements, err := processor.registerPatternRenames(patterns, false)
	if err != nil {
		return 0, err
	}
	processor.bodyStatements[bodyIndex] = statements
	return tokenIndex, nil
}

// findParamListStart returns the index of the ( that opens the parameters of
// the function at token tokenIndex. It skips a receiver, a name, and type
// parameters.
func (processor *processor) findParamListStart(tokenIndex int) (int, bool) {
	current := tokenIndex + 1
	if processor.tokenAt(current).is(token.LPAREN) {
		receiverEnd := processor.findClose(current)
		// A function literal can have a result type, as in func(x) T {.
		if receiverEnd < 0 || !processor.tokenAt(receiverEnd+1).is(token.IDENT) || !processor.tokenAt(receiverEnd+2).is(token.LPAREN) {
			return current, true
		}
		current = receiverEnd + 1
	}
	if processor.tokenAt(current).is(token.IDENT) {
		current++
	}
	if processor.tokenAt(current).is(token.LBRACK) {
		if current = processor.findClose(current); current < 0 {
			return 0, false
		}
		current++
	}
	return current, processor.tokenAt(current).is(token.LPAREN)
}

// paramRename replaces the tokens of a destructured parameter, up to token
// lastIndex and offset endOffset, with text.
type paramRename struct {
	lastIndex int
	endOffset int
	text      string
}

// registerPatternRenames schedules each pattern to become its generated name
// when the scan reaches it. So the output stays in source order. An untyped
// pattern also gets the placeholder type. It returns the statements for the
// start of the body.
func (processor *processor) registerPatternRenames(patterns []paramPattern, isUntyped bool) (paramStatements, error) {
	var statements paramStatements
	for _, pattern := range patterns {
		rename := paramRename{text: pattern.name + " "}
		switch {
		case isUntyped:
			closeIndex := processor.findClose(pattern.openIndex)
			rename = paramRename{lastIndex: closeIndex, endOffset: processor.tokens[closeIndex].end(), text: pattern.name + " " + UntypedMarker}
		case pattern.typeStart < 0:
			return paramStatements{}, &Error{pattern.open.offset, "a destructured parameter needs a type"}
		default:
			rename.lastIndex, rename.endOffset = pattern.typeStart-1, processor.tokens[pattern.typeStart].offset
		}
		if err := checkDefaults(pattern.bindings); err != nil {
			return paramStatements{}, err
		}
		processor.paramRenames[pattern.openIndex] = rename
		statements.add(pattern)
	}
	return statements, nil
}

// renameParam applies the rename registered for the pattern at token
// tokenIndex. It returns the index of the last token of the pattern.
func (processor *processor) renameParam(tokenIndex int) (int, bool) {
	rename, ok := processor.paramRenames[tokenIndex]
	if !ok {
		return 0, false
	}
	delete(processor.paramRenames, tokenIndex)
	processor.replace(processor.tokens[tokenIndex].offset, rename.endOffset, rename.text)
	return rename.lastIndex, true
}

// isUntypedParamList reports whether tokens [from, to) are only names and
// patterns, like "a, {b}", with no types.
func (processor *processor) isUntypedParamList(from, to int) bool {
	if from == to {
		return false // () needs no placeholder types
	}
	for start := from; start < to; {
		end := processor.findParamEnd(start, to)
		first := processor.tokens[start]
		isName := end == start+1 && first.is(token.IDENT)
		isPattern := (first.is(token.LBRACE) || first.is(token.LBRACK)) && processor.findClose(start) == end-1
		if !isName && !isPattern {
			return false
		}
		start = end + 1
	}
	return true
}

// markUntypedNames gives each plain name in tokens [from, to) the
// placeholder type. The names stay source text, so they map to the output.
func (processor *processor) markUntypedNames(from, to int) {
	for start := from; start < to; {
		end := processor.findParamEnd(start, to)
		if end == start+1 && processor.tokens[start].is(token.IDENT) {
			processor.inserts[end] += " " + UntypedMarker
		}
		start = end + 1
	}
}

func (statements *paramStatements) add(pattern paramPattern) {
	statements.text += " "
	statements.starts = append(statements.starts, len(statements.text))
	statements.text += formatAssignment(removeBlankElements(pattern.bindings), ":=", pattern.name) + ";"
}

// insertParamStatements adds statements at offset and records them.
func (processor *processor) insertParamStatements(offset int, statements paramStatements) {
	insertedAt := processor.replace(offset, offset, statements.text)
	for _, start := range statements.starts {
		processor.result.Params = append(processor.result.Params, insertedAt+start)
	}
}

// findParamPatterns finds the destructured parameters between the parentheses
// at tokens lparenIndex and rparenIndex.
func (processor *processor) findParamPatterns(lparenIndex, rparenIndex int) ([]paramPattern, error) {
	if rparenIndex < 0 {
		return nil, nil
	}
	var patterns []paramPattern
	for start := lparenIndex + 1; start < rparenIndex; {
		end := processor.findParamEnd(start, rparenIndex)
		open := processor.tokens[start]
		if open.is(token.LBRACE) || open.is(token.LBRACK) {
			pattern, ok, err := processor.parsePattern(start, end)
			if err != nil {
				return nil, err
			}
			if ok {
				patterns = append(patterns, pattern)
			}
		}
		start = end + 1
	}
	for patternIndex := range patterns {
		pattern := &patterns[patternIndex]
		pattern.name = processor.fresh(buildParamName(processor.getTypeName(pattern.typeStart)))
	}
	return patterns, nil
}

// findParamEnd returns the index of the , or ) that ends the parameter that
// starts at token start.
func (processor *processor) findParamEnd(start, rparenIndex int) int {
	depth := 0
	for tokenIndex := start; tokenIndex < rparenIndex; tokenIndex++ {
		switch current := processor.tokens[tokenIndex]; {
		case current.is(token.LPAREN) || current.is(token.LBRACK) || current.is(token.LBRACE):
			depth++
		case current.is(token.RPAREN) || current.is(token.RBRACK) || current.is(token.RBRACE):
			depth--
		case current.is(token.COMMA) && depth == 0:
			return tokenIndex
		}
	}
	return rparenIndex
}

// parsePattern parses the parameter in tokens [start, end) as a pattern,
// with an optional type after a colon.
func (processor *processor) parsePattern(start, end int) (paramPattern, bool, error) {
	open := processor.tokens[start]
	isArray := open.is(token.LBRACK)
	bindings, after, err := processor.pattern(start+1, isArray, "")
	if err != nil || bindings == nil {
		return paramPattern{}, false, err
	}
	pattern := paramPattern{openIndex: start, open: open, isArray: isArray, bindings: bindings, typeStart: -1}
	switch {
	case after == end:
	case processor.tokenAt(after).is(token.COLON) && after+1 < end:
		pattern.typeStart = after + 1
	default:
		return paramPattern{}, false, nil
	}
	return pattern, true, nil
}

// getTypeName returns the name of the type that starts at token typeStart,
// without pointers and a package. It returns "" if the type has no name.
func (processor *processor) getTypeName(typeStart int) string {
	if typeStart < 0 {
		return ""
	}
	for processor.tokenAt(typeStart).is(token.MUL) {
		typeStart++
	}
	if !processor.tokenAt(typeStart).is(token.IDENT) {
		return ""
	}
	if processor.tokenAt(typeStart+1).is(token.PERIOD) && processor.tokenAt(typeStart+2).is(token.IDENT) {
		typeStart += 2
	}
	return processor.tokenAt(typeStart).text
}

// buildParamName turns an exported type name into a parameter name, such as
// HTTPConfig into httpConfig. Other types get defaultParamName.
func buildParamName(typeName string) string {
	firstLetter, _ := utf8.DecodeRuneInString(typeName)
	if !unicode.IsUpper(firstLetter) {
		return defaultParamName
	}
	upperPrefixLen := 0
	for _, letter := range typeName {
		if !unicode.IsUpper(letter) {
			break
		}
		upperPrefixLen += utf8.RuneLen(letter)
	}
	// In HTTPConfig, the C starts the next word.
	if upperPrefixLen > utf8.RuneLen(firstLetter) && upperPrefixLen < len(typeName) {
		_, lastLetterSize := utf8.DecodeLastRuneInString(typeName[:upperPrefixLen])
		upperPrefixLen -= lastLetterSize
	}
	return strings.ToLower(typeName[:upperPrefixLen]) + typeName[upperPrefixLen:]
}

// findBodyStart returns the index of the { that opens the body of a function
// whose parameters close at token rparenIndex. It skips the results. It
// reports false for a function type, which has no body.
func (processor *processor) findBodyStart(rparenIndex int) (int, bool) {
	depth := 0
	for tokenIndex := rparenIndex + 1; tokenIndex < len(processor.tokens); tokenIndex++ {
		current := processor.tokens[tokenIndex]
		switch {
		case current.is(token.LBRACE) && depth == 0:
			previous := processor.tokens[tokenIndex-1]
			if !previous.is(token.STRUCT) && !previous.is(token.INTERFACE) {
				return tokenIndex, true
			}
			if tokenIndex = processor.findClose(tokenIndex); tokenIndex < 0 {
				return 0, false
			}
		case current.is(token.LPAREN) || current.is(token.LBRACK):
			depth++
		case current.is(token.RPAREN) || current.is(token.RBRACK):
			if depth--; depth < 0 {
				return 0, false
			}
		case depth == 0 && (current.is(token.SEMICOLON) || current.is(token.COMMA) || current.is(token.ASSIGN) || current.is(token.RBRACE)):
			return 0, false
		}
	}
	return 0, false
}

// findClose returns the index of the bracket that closes the bracket at
// token openIndex, or -1.
func (processor *processor) findClose(openIndex int) int {
	depth := 0
	for tokenIndex := openIndex; tokenIndex < len(processor.tokens); tokenIndex++ {
		switch processor.tokens[tokenIndex].kind {
		case token.LPAREN, token.LBRACK, token.LBRACE:
			depth++
		case token.RPAREN, token.RBRACK, token.RBRACE:
			if depth--; depth == 0 {
				return tokenIndex
			}
		}
	}
	return -1
}
