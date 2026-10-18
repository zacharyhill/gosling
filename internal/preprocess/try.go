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
		next, err := processor.catch(tokenIndex, tryStatement)
		if err != nil {
			return 0, err
		}
		processor.braceStack = append(processor.braceStack, frame{kind: frameCatch, tryIndex: closedFrame.tryIndex})
		return next, nil
	case frameCatch:
		tryStatement := &processor.result.Tries[closedFrame.tryIndex]
		switch next := processor.tokenAt(tokenIndex + 1); {
		case next.isIdent("finally"):
			return processor.finally(tokenIndex, tryStatement)
		case next.isIdent("catch"):
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
		if after := processor.tokenAt(tokenIndex + 2); processor.tokenAt(tokenIndex+1).is(token.SEMICOLON) && (after.isIdent("finally") || after.isIdent("catch")) {
			return 0, &Error{after.offset, after.text + " must be on the same line as the closing brace before it"}
		}
	}
	return tokenIndex, nil
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
	if lbrace := processor.tokenAt(tokenIndex + 2); lbrace.is(token.LBRACE) {
		tryStatement.CatchEnd = len("else {") + processor.replace(keyword.offset, lbrace.end(), "else {")
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
	text := fmt.Sprintf("else { var %s error; _ = %s;", name.text, name.text)
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
