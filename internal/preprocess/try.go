package preprocess

import (
	"fmt"
	"go/token"
)

// Try records one try/catch statement. Offsets are into Result.Src.
type Try struct {
	If       int // offset of the "if" that replaces "try"
	HasCatch bool
	CatchEnd int    // offset directly after the generated catch prologue
	Binding  string // catch variable name, empty for a bare catch
	// Finally is the offset of the opening brace of the finally block, or 0.
	// "finally" becomes ";", so the block is a separate statement after the try.
	Finally int
}

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
		if processor.tokenAt(tokenIndex + 1).isIdent("finally") {
			return processor.finally(tokenIndex, &processor.result.Tries[closedFrame.tryIndex])
		}
		if processor.tokenAt(tokenIndex+1).is(token.SEMICOLON) && processor.tokenAt(tokenIndex+2).isIdent("finally") {
			return 0, &Error{processor.tokenAt(tokenIndex + 2).offset, "finally must be on the same line as the closing brace before it"}
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
	if !lparen.is(token.LPAREN) || !name.is(token.IDENT) || !rparen.is(token.RPAREN) || !lbrace.is(token.LBRACE) {
		return 0, &Error{keyword.offset, "expected catch (name) or catch"}
	}
	// The declaration gives the catch variable a type for go/types. The
	// blank assignment stops go/types from reporting it as unused.
	text := fmt.Sprintf("else { var %s error; _ = %s;", name.text, name.text)
	tryStatement.CatchEnd = len(text) + processor.replace(keyword.offset, lbrace.end(), text)
	tryStatement.Binding = name.text
	return tokenIndex + 5, nil
}
