package preprocess

import (
	"fmt"
	"go/token"
	"strings"
)

// Try records one try/catch statement. Offsets are into Result.Src.
type Try struct {
	If       int // offset of the "if" that replaces "try"
	HasCatch bool
	// CatchEnd is the offset directly after the generated prologue of the
	// catch that catches all errors. It is 0 if each catch has a filter.
	CatchEnd int
	Binding  string // catch variable name, empty for a bare catch
	// HasPanic is true for a catch panic clause. Its block is the last else
	// branch. With a catch block too, the catch becomes "else if true {".
	HasPanic     bool
	PanicEnd     int    // offset directly after the generated catch panic prologue
	PanicBinding string // catch panic variable name, empty without one
	// Finally is the offset of the opening brace of the finally block, or 0.
	// "finally" becomes ";", so the block is a separate statement after the try.
	Finally int
	// Filters holds the catch clauses with a type or a value, in order.
	// Each becomes an "else if" before the catch that catches all errors.
	Filters []Filter
}

// Filter is a catch clause that catches only some errors:
//
//	catch (e: T)        ->  else if __goslingAs(new(T)) { var e T; _ = e;
//	catch (e is value)  ->  else if __goslingIs(value) { var e error; _ = e;
type Filter struct {
	Binding  string
	CatchEnd int // offset directly after the generated prologue
}

// The conditions of typed catch clauses type-check as calls to these markers.
const (
	AsMarker = "__goslingAs"
	IsMarker = "__goslingIs"
)

func (processor *processor) closeBrace(tokenIndex int) (int, error) {
	if len(processor.braceStack) == 0 {
		return tokenIndex, nil
	}
	closedFrame := processor.braceStack[len(processor.braceStack)-1]
	processor.braceStack = processor.braceStack[:len(processor.braceStack)-1]
	switch closedFrame.kind {
	case frameTry:
		tryStatement := &processor.result.Tries[closedFrame.tryIndex]
		if processor.tokenAt(tokenIndex + 1).isIdent("finally") {
			return processor.finally(tokenIndex, tryStatement)
		}
		if processor.isPanicCatch(tokenIndex + 1) {
			return processor.catchPanic(tokenIndex, closedFrame.tryIndex)
		}
		next, err := processor.catch(tokenIndex, tryStatement)
		if err != nil {
			return 0, err
		}
		processor.braceStack = append(processor.braceStack, frame{kind: frameCatch, tryIndex: closedFrame.tryIndex})
		return next, nil
	case frameCatch:
		tryStatement := &processor.result.Tries[closedFrame.tryIndex]
		if processor.isPanicCatch(tokenIndex + 1) {
			return processor.catchPanic(tokenIndex, closedFrame.tryIndex)
		}
		if next := processor.tokenAt(tokenIndex + 1); next.isIdent("catch") {
			if tryStatement.CatchEnd != 0 {
				return 0, &Error{next.offset, "only the last catch can catch all errors"}
			}
			lbraceIndex, err := processor.catch(tokenIndex, tryStatement)
			if err != nil {
				return 0, err
			}
			processor.braceStack = append(processor.braceStack, closedFrame)
			return lbraceIndex, nil
		}
		return processor.rewriteAfterHandler(tokenIndex, closedFrame)
	case framePanic:
		if processor.tokenAt(tokenIndex + 1).isIdent("catch") {
			return 0, &Error{processor.tokenAt(tokenIndex + 1).offset, "catch panic must be the last catch clause"}
		}
		return processor.rewriteAfterHandler(tokenIndex, closedFrame)
	}
	return tokenIndex, nil
}

// rewriteAfterHandler rewrites a finally clause after the closing brace of a
// catch or catch panic block at token tokenIndex. A finally or catch on the
// next line is an error.
func (processor *processor) rewriteAfterHandler(tokenIndex int, closedFrame frame) (int, error) {
	if processor.tokenAt(tokenIndex + 1).isIdent("finally") {
		return processor.finally(tokenIndex, &processor.result.Tries[closedFrame.tryIndex])
	}
	if next := processor.tokenAt(tokenIndex + 2); processor.tokenAt(tokenIndex+1).is(token.SEMICOLON) && (next.isIdent("finally") || next.isIdent("catch")) {
		return 0, &Error{next.offset, next.text + " must be on the same line as the closing brace before it"}
	}
	return tokenIndex, nil
}

func (processor *processor) isPanicCatch(tokenIndex int) bool {
	return processor.tokenAt(tokenIndex).isIdent("catch") && processor.tokenAt(tokenIndex+1).isIdent("panic")
}

// isPanicCatchAfter reports whether a catch panic clause follows the block
// that opens at token lbraceIndex.
func (processor *processor) isPanicCatchAfter(lbraceIndex int) bool {
	depth := 0
	for tokenIndex := lbraceIndex; tokenIndex < len(processor.tokens); tokenIndex++ {
		switch processor.tokens[tokenIndex].kind {
		case token.LBRACE:
			depth++
		case token.RBRACE:
			depth--
			if depth == 0 {
				return processor.isPanicCatch(tokenIndex + 1)
			}
		}
	}
	return false
}

// catchPanic rewrites the catch panic clause after the block that closes at
// token tokenIndex. It returns the index of the opening brace of its block.
func (processor *processor) catchPanic(tokenIndex, tryIndex int) (int, error) {
	tryStatement := &processor.result.Tries[tryIndex]
	keyword := processor.tokenAt(tokenIndex + 1)
	if len(tryStatement.Filters) > 0 {
		return 0, &Error{keyword.offset, "catch panic cannot follow a typed catch clause"}
	}
	tryStatement.HasPanic = true
	lbraceIndex := tokenIndex + 3
	prologueText := "else {"
	if !processor.tokenAt(tokenIndex + 3).is(token.LBRACE) {
		lparen, name, rparen := processor.tokenAt(tokenIndex+3), processor.tokenAt(tokenIndex+4), processor.tokenAt(tokenIndex+5)
		lbraceIndex = tokenIndex + 6
		if !lparen.is(token.LPAREN) || !name.is(token.IDENT) || !rparen.is(token.RPAREN) || !processor.tokenAt(lbraceIndex).is(token.LBRACE) {
			return 0, &Error{keyword.offset, "expected catch panic (name) or catch panic"}
		}
		prologueText = fmt.Sprintf("else { var %s any; _ = %s;", name.text, name.text)
		tryStatement.PanicBinding = name.text
	}
	tryStatement.PanicEnd = len(prologueText) + processor.replace(keyword.offset, processor.tokenAt(lbraceIndex).end(), prologueText)
	processor.braceStack = append(processor.braceStack, frame{kind: framePanic, tryIndex: tryIndex})
	return lbraceIndex, nil
}

// finally rewrites the "finally {" after the brace at token tokenIndex into
// "; {". It returns the index of the opening brace of the finally block.
func (processor *processor) finally(tokenIndex int, tryStatement *Try) (int, error) {
	keyword, lbrace := processor.tokenAt(tokenIndex+1), processor.tokenAt(tokenIndex+2)
	if !lbrace.is(token.LBRACE) {
		return 0, &Error{keyword.offset, "finally must be followed by a block"}
	}
	tryStatement.Finally = processor.replace(keyword.offset, lbrace.end(), "; {") + len("; ")
	processor.braceStack = append(processor.braceStack, frame{kind: frameBlock})
	return tokenIndex + 2, nil
}

// catch rewrites the catch clause after the try block that closes at token
// tokenIndex. It returns the index of the opening brace of the catch block.
func (processor *processor) catch(tokenIndex int, tryStatement *Try) (int, error) {
	keyword := processor.tokenAt(tokenIndex + 1)
	if keyword.is(token.SEMICOLON) && (processor.tokenAt(tokenIndex+2).isIdent("catch") || processor.tokenAt(tokenIndex+2).isIdent("finally")) {
		return 0, &Error{processor.tokenAt(tokenIndex + 2).offset, processor.tokenAt(tokenIndex+2).text + " must be on the same line as the closing brace of try"}
	}
	if !keyword.isIdent("catch") {
		return 0, &Error{processor.tokens[tokenIndex].offset, "try block must be followed by catch or finally"}
	}
	tryStatement.HasCatch = true
	// With a catch panic clause, the catch block is the middle branch of
	// an if-else chain. Then go/types sees which branches terminate.
	elseText := "else {"
	if lbrace := processor.tokenAt(tokenIndex + 2); lbrace.is(token.LBRACE) {
		if processor.isPanicCatchAfter(tokenIndex + 2) {
			elseText = "else if true {"
		}
		tryStatement.CatchEnd = len(elseText) + processor.replace(keyword.offset, lbrace.end(), elseText)
		return tokenIndex + 2, nil
	}
	lparen, name, rparen, lbrace := processor.tokenAt(tokenIndex+2), processor.tokenAt(tokenIndex+3), processor.tokenAt(tokenIndex+4), processor.tokenAt(tokenIndex+5)
	if lparen.is(token.LPAREN) && name.is(token.IDENT) && (rparen.is(token.COLON) || rparen.isIdent("is")) {
		return processor.rewriteCatchFilter(tokenIndex+1, tryStatement)
	}
	if !lparen.is(token.LPAREN) || !name.is(token.IDENT) || !rparen.is(token.RPAREN) || !lbrace.is(token.LBRACE) {
		return 0, &Error{keyword.offset, errCatchSyntax}
	}
	// The declaration gives the catch variable a type for go/types. The
	// blank assignment stops go/types from reporting it as unused.
	if processor.isPanicCatchAfter(tokenIndex + 5) {
		elseText = "else if true {"
	}
	text := fmt.Sprintf("%s var %s error; _ = %s;", elseText, name.text, name.text)
	tryStatement.CatchEnd = len(text) + processor.replace(keyword.offset, lbrace.end(), text)
	tryStatement.Binding = name.text
	return tokenIndex + 5, nil
}

const errCatchSyntax = "expected catch, catch (name), catch (name: Type), or catch (name is value)"

// rewriteCatchFilter rewrites the catch clause with a type or a value whose
// keyword is at token keywordIndex. It returns the index of the opening
// brace of the catch block.
func (processor *processor) rewriteCatchFilter(keywordIndex int, tryStatement *Try) (int, error) {
	keyword, name, separator := processor.tokens[keywordIndex], processor.tokenAt(keywordIndex+2), processor.tokenAt(keywordIndex+3)
	rparenIndex, ok := processor.findClosingParen(keywordIndex + 1)
	lbrace := processor.tokenAt(rparenIndex + 1)
	if !ok || rparenIndex == keywordIndex+4 || !lbrace.is(token.LBRACE) {
		return 0, &Error{keyword.offset, errCatchSyntax}
	}
	rparen, filterStart := processor.tokens[rparenIndex], processor.tokenAt(keywordIndex+4)
	var conditionStart, prologue string
	if separator.isIdent("is") {
		conditionStart = "else if " + IsMarker + "("
		prologue = fmt.Sprintf(") { var %s error; _ = %s;", name.text, name.text)
	} else {
		typeText := string(processor.src[filterStart.offset:rparen.offset])
		if strings.Contains(typeText, "\n") {
			return 0, &Error{filterStart.offset, "a catch type must be on one line"}
		}
		conditionStart = "else if " + AsMarker + "(new("
		prologue = fmt.Sprintf(")) { var %s %s; _ = %s;", name.text, strings.TrimSpace(typeText), name.text)
	}
	processor.replace(keyword.offset, filterStart.offset, conditionStart)
	catchEnd := len(prologue) + processor.replace(rparen.offset, lbrace.end(), prologue)
	tryStatement.HasCatch = true
	tryStatement.Filters = append(tryStatement.Filters, Filter{Binding: name.text, CatchEnd: catchEnd})
	return rparenIndex + 1, nil
}

// findClosingParen returns the index of the ) that closes the ( at token
// lparenIndex.
func (processor *processor) findClosingParen(lparenIndex int) (int, bool) {
	depth := 0
	for tokenIndex := lparenIndex; tokenIndex < len(processor.tokens); tokenIndex++ {
		switch processor.tokens[tokenIndex].kind {
		case token.LPAREN, token.LBRACK, token.LBRACE:
			depth++
		case token.RPAREN, token.RBRACK, token.RBRACE:
			depth--
		}
		if depth == 0 {
			return tokenIndex, processor.tokens[tokenIndex].is(token.RPAREN)
		}
	}
	return 0, false
}
