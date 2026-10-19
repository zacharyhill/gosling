package preprocess

import (
	"bytes"
	"fmt"
	"go/token"
	"strings"
	"unicode"
	"unicode/utf8"
)

// implBlock holds the token indexes that an impl block rewrites. An unused
// index is -1.
type implBlock struct {
	typeText           string // the receiver type, such as Stack[T]
	closeIndex         int
	funcKeywordIndexes map[int]bool
	constructorIndex   int
	constructorOpen    int // body braces of a constructor without results
	constructorClose   int
	// constructorStatements declare the destructured parameters of a
	// constructor without results. They follow its prologue.
	constructorStatements *paramStatements
	constructorName       string
}

// rewrites reports whether the block rewrites the token at tokenIndex. The
// FUNC case of rewrite adds the receivers of methods.
func (block *implBlock) rewrites(tokenIndex int) bool {
	switch tokenIndex {
	case block.closeIndex, block.constructorIndex, block.constructorOpen, block.constructorClose:
		return true
	}
	return false
}

// isImpl reports whether the token at tokenIndex starts an impl block. Go
// has no package-level statement that starts with an identifier, so
// `impl Name` at package level is always an impl block.
func (processor *processor) isImpl(tokenIndex int) bool {
	return processor.tokens[tokenIndex].isIdent("impl") && processor.tokenAt(tokenIndex+1).is(token.IDENT) &&
		len(processor.braceStack) == 0 && processor.parens == 0 && processor.atStmtStart(tokenIndex)
}

// rewriteImplHeader rewrites the header `impl Name[T] implements A, B {` at tokenIndex
// into one interface check for each interface. It removes the braces of the
// block and records the members that later tokens rewrite. It returns the
// index of the opening brace.
func (processor *processor) rewriteImplHeader(tokenIndex int) (int, error) {
	keyword, name := processor.tokens[tokenIndex], processor.tokens[tokenIndex+1]
	block := &implBlock{typeText: name.text, closeIndex: -1, funcKeywordIndexes: map[int]bool{}, constructorIndex: -1, constructorOpen: -1, constructorClose: -1}
	openIndex := tokenIndex + 2
	isGeneric := processor.tokenAt(openIndex).is(token.LBRACK)
	if isGeneric {
		closeIndex, ok := processor.matchBracket(openIndex)
		if !ok {
			return 0, &Error{keyword.offset, "unbalanced brackets"}
		}
		block.typeText = string(processor.src[name.offset:processor.tokens[closeIndex].end()])
		openIndex = closeIndex + 1
	}
	interfaces, openIndex, err := processor.parseImplements(openIndex)
	if err != nil {
		return 0, err
	}
	if len(interfaces) > 0 && isGeneric {
		return 0, &Error{keyword.offset, "a generic impl block cannot use implements"}
	}
	open := processor.tokenAt(openIndex)
	if !open.is(token.LBRACE) {
		return 0, &Error{keyword.offset, fmt.Sprintf("expected { after impl %s", name.text)}
	}
	block.closeIndex = processor.findClose(openIndex)
	if block.closeIndex < 0 {
		return 0, &Error{open.offset, "impl block is not closed"}
	}
	block.constructorName = buildConstructorName(name.text)
	if err := processor.recordImplMembers(block, openIndex+1, isGeneric); err != nil {
		return 0, err
	}
	processor.emitInterfaceChecks(keyword.offset, open.end(), block.typeText, interfaces)
	processor.currentImpl = block
	return openIndex, nil
}

// parseImplements parses `implements A, pkg.B` at tokenIndex. It returns the
// interface names and the index of the token after them.
func (processor *processor) parseImplements(tokenIndex int) ([]string, int, error) {
	if !processor.tokenAt(tokenIndex).isIdent("implements") {
		return nil, tokenIndex, nil
	}
	var interfaces []string
	for {
		tokenIndex++
		start := processor.tokenAt(tokenIndex)
		if !start.is(token.IDENT) {
			return nil, 0, &Error{start.offset, "implements needs interface names"}
		}
		if processor.tokenAt(tokenIndex+1).is(token.PERIOD) && processor.tokenAt(tokenIndex+2).is(token.IDENT) {
			tokenIndex += 2
		}
		interfaces = append(interfaces, string(processor.src[start.offset:processor.tokens[tokenIndex].end()]))
		tokenIndex++
		if !processor.tokenAt(tokenIndex).is(token.COMMA) {
			return interfaces, tokenIndex, nil
		}
	}
}

// emitInterfaceChecks replaces src[start:end] with `var _ I = (*T)(nil); `
// for each interface I.
func (processor *processor) emitInterfaceChecks(start, end int, typeText string, interfaces []string) {
	var checks strings.Builder
	for _, name := range interfaces {
		fmt.Fprintf(&checks, "var _ %s = (*%s)(nil); ", name, typeText)
	}
	offset := processor.replace(start, end, checks.String())
	for _, name := range interfaces {
		offset += len(fmt.Sprintf("var _ %s = (*%s)(nil)", name, typeText))
		processor.result.Splits = append(processor.result.Splits, offset)
		offset += len("; ")
	}
}

// recordImplMembers records the methods and the constructor of the impl block
// whose members start at token tokenIndex.
func (processor *processor) recordImplMembers(block *implBlock, tokenIndex int, isGeneric bool) error {
	for tokenIndex < block.closeIndex {
		if processor.tokens[tokenIndex].is(token.SEMICOLON) {
			tokenIndex++
			continue
		}
		end, body := processor.findMemberEnd(tokenIndex, block.closeIndex)
		if err := processor.recordImplMember(block, tokenIndex, body, isGeneric); err != nil {
			return err
		}
		tokenIndex = end
	}
	return nil
}

// findMemberEnd returns the index of the token after the member that starts at
// tokenIndex, and the index of the last top-level { in the member, or -1.
func (processor *processor) findMemberEnd(tokenIndex, limit int) (int, int) {
	body := -1
	for ; tokenIndex < limit && !processor.tokens[tokenIndex].is(token.SEMICOLON); tokenIndex++ {
		switch processor.tokens[tokenIndex].kind {
		case token.LBRACE:
			body = tokenIndex
			tokenIndex = processor.findClose(tokenIndex)
		case token.LPAREN, token.LBRACK:
			tokenIndex = processor.findClose(tokenIndex)
		}
	}
	return tokenIndex, body
}

// recordImplMember records the member that starts at token tokenIndex.
// body is the index of its last top-level {, or -1.
func (processor *processor) recordImplMember(block *implBlock, tokenIndex, body int, isGeneric bool) error {
	member, next := processor.tokens[tokenIndex], processor.tokenAt(tokenIndex+1)
	switch {
	case member.is(token.FUNC) && next.is(token.IDENT):
		block.funcKeywordIndexes[tokenIndex] = true
	case member.is(token.FUNC) && next.is(token.LPAREN):
		return &Error{member.offset, "a method in an impl block cannot have a receiver; use this in its body"}
	case member.isIdent("constructor") && next.is(token.LPAREN):
		if block.constructorIndex >= 0 {
			return &Error{member.offset, "an impl block can have only one constructor"}
		}
		if isGeneric {
			return &Error{member.offset, "a generic impl block cannot have a constructor"}
		}
		block.constructorIndex = tokenIndex
		hasResults := body != processor.findClose(tokenIndex+1)+1
		if body >= 0 && !hasResults {
			block.constructorOpen, block.constructorClose = body, processor.findClose(body)
		}
	default:
		return &Error{member.offset, "an impl block can only contain methods and a constructor"}
	}
	return nil
}

// rewriteImplToken rewrites a token that the current impl block recorded.
func (processor *processor) rewriteImplToken(tokenIndex int, pendingTry *int) (int, error) {
	block, current := processor.currentImpl, processor.tokens[tokenIndex]
	switch {
	case tokenIndex == block.closeIndex:
		processor.replace(current.offset, current.end(), "")
		processor.currentImpl = nil
	case tokenIndex == block.constructorIndex:
		return processor.rewriteConstructorHeader(block, tokenIndex)
	case tokenIndex == block.constructorOpen:
		processor.writeConstructorPrologue(block, tokenIndex)
		processor.openBracket(current, pendingTry)
	case tokenIndex == block.constructorClose:
		if processor.hasLineBreakBetween(processor.tokens[tokenIndex-1].end(), current.offset) {
			processor.replace(current.offset, current.end(), "return this }")
		} else {
			start := processor.replace(current.offset, current.end(), "; return this }")
			processor.result.Splits = append(processor.result.Splits, start)
		}
		return processor.closeBrace(tokenIndex)
	}
	return tokenIndex, nil
}

// rewriteConstructorHeader rewrites the constructor keyword at tokenIndex
// and its parameters. It keeps the statements of destructured parameters for
// the prologue.
func (processor *processor) rewriteConstructorHeader(block *implBlock, tokenIndex int) (int, error) {
	current := processor.tokens[tokenIndex]
	processor.replace(current.offset, current.end(), "func "+block.constructorName)
	if err := processor.signature(tokenIndex); err != nil {
		return 0, err
	}
	next, err := processor.rewriteFuncParams(tokenIndex)
	if statements, ok := processor.bodyStatements[block.constructorOpen]; ok {
		delete(processor.bodyStatements, block.constructorOpen)
		block.constructorStatements = &statements
	}
	return next, err
}

// writeConstructorPrologue replaces the { at tokenIndex with the result type
// and the statement that makes this.
func (processor *processor) writeConstructorPrologue(block *implBlock, tokenIndex int) {
	current := processor.tokens[tokenIndex]
	prologue := fmt.Sprintf("*%s { this := &%s{}", block.typeText, block.typeText)
	// Parameter statements go after the prologue on the same line.
	if processor.hasLineBreakBetween(current.end(), processor.tokenAt(tokenIndex+1).offset) && block.constructorStatements == nil {
		processor.replace(current.offset, current.end(), prologue)
	} else {
		start := processor.replace(current.offset, current.end(), prologue+"; ")
		processor.result.Splits = append(processor.result.Splits, start+len(prologue))
	}
	if block.constructorStatements != nil {
		processor.insertParamStatements(current.end(), *block.constructorStatements)
	}
}

// addImplReceiver adds the receiver of the current impl block after the
// func keyword of a method. The keyword stays, so outline ranges start at it.
func (processor *processor) addImplReceiver(funcKeyword lexeme) {
	processor.replace(funcKeyword.end(), funcKeyword.end(), fmt.Sprintf(" (this *%s)", processor.currentImpl.typeText))
}

func (processor *processor) hasLineBreakBetween(start, end int) bool {
	return bytes.IndexByte(processor.src[start:end], '\n') >= 0
}

// buildConstructorName follows the Go convention: NewUser for User, newConfig
// for config.
func buildConstructorName(typeName string) string {
	firstRune, _ := utf8.DecodeRuneInString(typeName)
	if unicode.IsUpper(firstRune) {
		return "New" + typeName
	}
	return "new" + upperFirst(typeName)
}
