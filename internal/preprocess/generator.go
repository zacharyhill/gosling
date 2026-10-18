package preprocess

import (
	"go/token"
	"strings"
)

// A generator body becomes a Go 1.23 iterator. The yield type comes from
// the text of the result type, so no type checking is necessary:
//
//	func* f() iter.Seq[int] {      func f() iter.Seq[int] { return func(yield func(int) bool) {
//		yield x                        if !yield(x) { return }
//	}                              }}
type iteratorType struct {
	yieldType string
	isPair    bool // the result is iter.Seq2
	// paramStatements declare the destructured parameters. They follow the
	// function literal that opens the body.
	paramStatements *paramStatements
}

// pendingYield is a yield statement that the scan has not finished. closer
// is the text to add at the end of the statement.
type pendingYield struct {
	start  nesting
	closer string
}

func (processor *processor) isGeneratorKeyword(tokenIndex int) bool {
	return processor.tokens[tokenIndex].is(token.FUNC) && processor.tokenAt(tokenIndex+1).is(token.MUL) && processor.touchesNext(tokenIndex)
}

// generator rewrites the "func*" at token tokenIndex into "func". It records
// the generator for the opening brace of its body, and returns its index.
func (processor *processor) generator(tokenIndex int) (int, error) {
	keyword, star := processor.tokens[tokenIndex], processor.tokens[tokenIndex+1]
	lbraceIndex := processor.findBodyBrace(tokenIndex + 2)
	if lbraceIndex < 0 {
		return 0, &Error{keyword.offset, "a generator needs a body"}
	}
	iterator, ok := processor.parseIteratorResult(lbraceIndex)
	if !ok {
		return 0, &Error{keyword.offset, "a generator must return iter.Seq[T] or iter.Seq2[K, V]"}
	}
	processor.iteratorByBrace[processor.tokens[lbraceIndex].offset] = iterator
	processor.replace(keyword.offset, star.end(), "func")
	return lbraceIndex, nil
}

// moveParamStatements moves the statements of the destructured parameters of
// the generator with the body brace at lbraceIndex after its function literal.
func (processor *processor) moveParamStatements(lbraceIndex int) {
	statements, ok := processor.bodyStatements[lbraceIndex]
	if !ok {
		return
	}
	delete(processor.bodyStatements, lbraceIndex)
	offset := processor.tokens[lbraceIndex].offset
	iterator := processor.iteratorByBrace[offset]
	iterator.paramStatements = &statements
	processor.iteratorByBrace[offset] = iterator
}

// findBodyBrace returns the index of the first { outside brackets from token
// tokenIndex, or -1 if the declaration ends first.
func (processor *processor) findBodyBrace(tokenIndex int) int {
	depth := 0
	for ; tokenIndex < len(processor.tokens); tokenIndex++ {
		switch current := processor.tokens[tokenIndex]; {
		case current.is(token.LPAREN) || current.is(token.LBRACK):
			depth++
		case current.is(token.RPAREN) || current.is(token.RBRACK):
			depth--
		case depth == 0 && current.is(token.LBRACE):
			return tokenIndex
		case depth == 0 && current.is(token.SEMICOLON):
			return -1
		}
	}
	return -1
}

// parseIteratorResult reads the result type `iter.Seq[...]` or `iter.Seq2[...]`
// that ends before the brace at token lbraceIndex.
func (processor *processor) parseIteratorResult(lbraceIndex int) (iteratorType, bool) {
	rbrackIndex := lbraceIndex - 1
	if !processor.tokenAt(rbrackIndex).is(token.RBRACK) {
		return iteratorType{}, false
	}
	lbrackIndex, depth := -1, 0
	for tokenIndex := rbrackIndex; tokenIndex >= 0 && lbrackIndex < 0; tokenIndex-- {
		switch processor.tokens[tokenIndex].kind {
		case token.RBRACK:
			depth++
		case token.LBRACK:
			depth--
			if depth == 0 {
				lbrackIndex = tokenIndex
			}
		}
	}
	if lbrackIndex < 3 || !processor.tokens[lbrackIndex-2].is(token.PERIOD) || !processor.tokens[lbrackIndex-3].isIdent("iter") {
		return iteratorType{}, false
	}
	typeName := processor.tokens[lbrackIndex-1]
	if !typeName.isIdent("Seq") && !typeName.isIdent("Seq2") {
		return iteratorType{}, false
	}
	typeArguments := string(processor.src[processor.tokens[lbrackIndex].end():processor.tokens[rbrackIndex].offset])
	return iteratorType{yieldType: "func(" + strings.TrimSpace(typeArguments) + ") bool", isPair: typeName.text == "Seq2"}, true
}

func (processor *processor) isGeneratorBody(lbrace lexeme) bool {
	_, ok := processor.iteratorByBrace[lbrace.offset]
	return ok
}

// openGeneratorBody opens the body of a generator with the brace at token
// tokenIndex.
func (processor *processor) openGeneratorBody(tokenIndex int, iterator iteratorType) {
	lbrace := processor.tokens[tokenIndex]
	textStart := processor.replace(lbrace.offset, lbrace.end(), "{ return func(yield "+iterator.yieldType+") {")
	processor.result.Generators = append(processor.result.Generators, textStart+len("{ return "))
	if iterator.paramStatements != nil {
		processor.insertParamStatements(lbrace.end(), *iterator.paramStatements)
	}
	processor.braceStack = append(processor.braceStack, frame{kind: frameGenerator, iterator: iterator})
}

// findInnerIterator returns the iterator type of the innermost generator
// body around the scan.
func (processor *processor) findInnerIterator() (iteratorType, bool) {
	for frameIndex := len(processor.braceStack) - 1; frameIndex >= 0; frameIndex-- {
		if processor.braceStack[frameIndex].kind == frameGenerator {
			return processor.braceStack[frameIndex].iterator, true
		}
	}
	return iteratorType{}, false
}

func (processor *processor) isInGenerator() bool {
	_, ok := processor.findInnerIterator()
	return ok
}

// yield rewrites the yield statement at token tokenIndex:
//
//	yield v       ->  if !yield(v) { return }
//	yield* seq    ->  for v := range seq { if !yield(v) { return } }
//
// It returns the index of the last token it uses.
func (processor *processor) yield(tokenIndex int) (int, error) {
	iterator, _ := processor.findInnerIterator()
	keyword, next := processor.tokens[tokenIndex], processor.tokenAt(tokenIndex+1)
	lastIndex, opening, closer := tokenIndex, "if !yield(", ") { return }"
	if next.is(token.MUL) && processor.touchesNext(tokenIndex) {
		variables := processor.fresh("v")
		if iterator.isPair {
			variables = processor.fresh("k") + ", " + variables
		}
		lastIndex, opening, closer = tokenIndex+1, "for "+variables+" := range ", " { if !yield("+variables+") { return } }"
	}
	switch processor.tokenAt(lastIndex + 1).kind {
	case token.SEMICOLON, token.RBRACE, token.EOF:
		return 0, &Error{keyword.offset, "yield needs a value"}
	}
	processor.result.Yields = append(processor.result.Yields, processor.replace(keyword.offset, processor.tokenAt(lastIndex+1).offset, opening))
	processor.pendingYields = append(processor.pendingYields, pendingYield{start: processor.nesting(), closer: closer})
	return lastIndex, nil
}

// finishYields closes the pending yield statements that end at token
// tokenIndex.
func (processor *processor) finishYields(tokenIndex int) {
	for len(processor.pendingYields) > 0 {
		pending := processor.pendingYields[len(processor.pendingYields)-1]
		switch processor.tokenAt(tokenIndex).kind {
		case token.SEMICOLON, token.RBRACE, token.EOF:
		default:
			return
		}
		if processor.nesting() != pending.start {
			return
		}
		processor.pendingYields = processor.pendingYields[:len(processor.pendingYields)-1]
		end := processor.tokenAt(tokenIndex - 1).end()
		processor.replace(end, end, pending.closer)
	}
}
