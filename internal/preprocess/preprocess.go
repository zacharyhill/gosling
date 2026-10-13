// Package preprocess changes gosling syntax into valid Go for the Go parser
// and type checker. It does not add or remove newlines, so line numbers do
// not change.
package preprocess

import (
	"fmt"
	"go/scanner"
	"go/token"
	"strings"
)

// MapMarker replaces the map keyword in a call like xs.map(f). Go cannot
// parse that call.
const MapMarker = "__goslingMap"

// TMarker is the function that replaces a template literal. t`a ${x}`
// becomes __goslingT(`a `, x, “). Text and expressions alternate. The
// first and last arguments are text.
const TMarker = "__goslingT"

type Result struct {
	Src    []byte
	Tries  []Try
	Unions []Union
	// Coalesce holds the offsets of the "||" operators that replace "??".
	Coalesce []int
	// Splits holds the offsets of added "; " separators. The statements on
	// each side belong on separate lines.
	Splits []int
	// Consts holds the offsets of the names that a const declares in a
	// for-of loop, a header, or a destructuring pattern.
	Consts []int
	edits  []edit
}

// Error is a syntax error at an offset into the original source.
type Error struct {
	Offset int
	Msg    string
}

func (syntaxError *Error) Error() string {
	return fmt.Sprintf("offset %d: %s", syntaxError.Offset, syntaxError.Msg)
}

type edit struct {
	origStart, origEnd int
	newStart, newEnd   int
}

type lexeme struct {
	offset int
	kind   token.Token
	text   string
}

func (lexeme lexeme) is(kind token.Token) bool { return lexeme.kind == kind }

func (lexeme lexeme) isIdent(name string) bool {
	return lexeme.kind == token.IDENT && lexeme.text == name
}

func (lexeme lexeme) isAutoSemicolon() bool {
	return lexeme.kind == token.SEMICOLON && lexeme.text != ";"
}

func (lexeme lexeme) end() int {
	switch {
	case lexeme.isAutoSemicolon():
		return lexeme.offset
	case lexeme.kind == token.SEMICOLON:
		return lexeme.offset + 1
	case lexeme.text != "":
		return lexeme.offset + len(lexeme.text)
	}
	return lexeme.offset + len(lexeme.kind.String())
}

type frameKind int

type frame struct {
	kind     frameKind
	tryIndex int // index into Result.Tries for frameTry and frameCatch
}

// nesting records the bracket depth where a pending rewrite starts. The
// rewrite ends at the first terminator at the same depth.
type nesting struct {
	braces, parens int
}

func (processor *processor) nesting() nesting {
	return nesting{braces: len(processor.braceStack), parens: processor.parens}
}

const (
	frameBlock frameKind = iota
	frameTry
	frameCatch
)

type processor struct {
	src         []byte
	tokens      []lexeme
	output      []byte
	copiedUpTo  int
	result      *Result
	identifiers map[string]bool
	braceStack  []frame
	parens      int // depth of open ( and [
	// scheduled maps a token index to replacement text. A rewrite uses it
	// when it ends at a token that a scan ahead found.
	scheduled           map[int]string
	pendingDestructures []*destructure
	pendingArrows       []pendingArrow
	// inserts maps a token index to text to add before that token.
	inserts map[int]string
}

// Process rewrites the gosling syntax in src into Go source with marker
// identifiers.
func Process(src []byte) (*Result, error) {
	processor := &processor{src: src, tokens: tokenize(src), result: &Result{}, identifiers: map[string]bool{}, scheduled: map[int]string{}, inserts: map[int]string{}}
	for _, current := range processor.tokens {
		if current.kind == token.IDENT {
			processor.identifiers[current.text] = true
		}
	}
	if err := processor.run(); err != nil {
		return nil, err
	}
	processor.output = append(processor.output, src[processor.copiedUpTo:]...)
	processor.result.Src = processor.output
	return processor.result, nil
}

func tokenize(src []byte) []lexeme {
	fileSet := token.NewFileSet()
	tokenFile := fileSet.AddFile("", -1, len(src))
	var sourceScanner scanner.Scanner
	// Ignore scanner errors. The scanner reports "?" as ILLEGAL. go/parser
	// reports real syntax errors later, with better messages.
	sourceScanner.Init(tokenFile, src, func(token.Position, string) {}, 0)
	var lexemes []lexeme
	for {
		pos, kind, literal := sourceScanner.Scan()
		if kind == token.EOF {
			return lexemes
		}
		lexemes = append(lexemes, lexeme{offset: tokenFile.Offset(pos), kind: kind, text: literal})
	}
}

func (processor *processor) replace(start, end int, text string) int {
	processor.output = append(processor.output, processor.src[processor.copiedUpTo:start]...)
	newStart := len(processor.output)
	processor.output = append(processor.output, text...)
	processor.result.edits = append(processor.result.edits, edit{start, end, newStart, len(processor.output)})
	processor.copiedUpTo = end
	return newStart
}

func (processor *processor) tokenAt(tokenIndex int) lexeme {
	if tokenIndex < len(processor.tokens) {
		return processor.tokens[tokenIndex]
	}
	return lexeme{offset: len(processor.src), kind: token.EOF}
}

func (processor *processor) run() error {
	if err := processor.rewriteTokens(); err != nil {
		return err
	}
	processor.finishArrows(len(processor.tokens))
	return processor.finishDestructure(len(processor.tokens))
}

// rewriteInterpolation rewrites gosling syntax in src[from:to]. That range
// is an expression inside a template literal.
func (processor *processor) rewriteInterpolation(from, to int) error {
	savedTokens, savedScheduled, savedInserts, savedDestructures, savedArrows := processor.tokens, processor.scheduled, processor.inserts, processor.pendingDestructures, processor.pendingArrows
	defer func() {
		processor.tokens, processor.scheduled, processor.inserts, processor.pendingDestructures, processor.pendingArrows = savedTokens, savedScheduled, savedInserts, savedDestructures, savedArrows
	}()
	processor.tokens = tokenize(processor.src[from:to])
	for len(processor.tokens) > 0 && processor.tokens[len(processor.tokens)-1].isAutoSemicolon() {
		processor.tokens = processor.tokens[:len(processor.tokens)-1]
	}
	for tokenIndex := range processor.tokens {
		processor.tokens[tokenIndex].offset += from
	}
	processor.scheduled, processor.inserts, processor.pendingDestructures, processor.pendingArrows = map[int]string{}, map[int]string{}, nil, nil
	if err := processor.rewriteTokens(); err != nil {
		return err
	}
	processor.finishArrows(len(processor.tokens))
	return nil
}

func (processor *processor) rewriteTokens() error {
	pendingTry := -1
	for tokenIndex := 0; tokenIndex < len(processor.tokens); tokenIndex++ {
		current := processor.tokens[tokenIndex]
		if text, ok := processor.inserts[tokenIndex]; ok {
			processor.replace(current.offset, current.offset, text)
			delete(processor.inserts, tokenIndex)
		}
		if text, ok := processor.scheduled[tokenIndex]; ok {
			processor.replace(current.offset, current.end(), text)
			delete(processor.scheduled, tokenIndex)
		}
		processor.finishArrows(tokenIndex)
		if err := processor.finishDestructure(tokenIndex); err != nil {
			return err
		}
		next, err := processor.rewrite(tokenIndex, &pendingTry)
		if err != nil {
			return err
		}
		tokenIndex = next
	}
	return nil
}

// rewrite handles the token at tokenIndex. It returns the index of the last
// token it uses.
func (processor *processor) rewrite(tokenIndex int, pendingTry *int) (int, error) {
	current := processor.tokens[tokenIndex]
	switch {
	case current.isIdent("try") && processor.tokenAt(tokenIndex+1).is(token.LBRACE) && processor.atStmtStart(tokenIndex):
		processor.result.Tries = append(processor.result.Tries, Try{If: processor.replace(current.offset, current.end(), "if true")})
		*pendingTry = len(processor.result.Tries) - 1
	case current.is(token.ILLEGAL) && current.text == "?":
		return processor.questionMark(tokenIndex)
	case current.is(token.TYPE):
		return processor.union(tokenIndex)
	case (current.isIdent("let") || current.is(token.CONST)) && (processor.atStmtStart(tokenIndex) || processor.inHeader(tokenIndex)):
		return processor.declaration(tokenIndex)
	case current.is(token.PERIOD) && processor.tokenAt(tokenIndex+1).is(token.MAP) && processor.tokenAt(tokenIndex+1).offset == current.end():
		processor.replace(processor.tokenAt(tokenIndex+1).offset, processor.tokenAt(tokenIndex+1).end(), MapMarker)
		return tokenIndex + 1, nil
	case current.is(token.ASSIGN) && processor.tokenAt(tokenIndex+1).is(token.GTR) && processor.tokenAt(tokenIndex+1).offset == current.end():
		return processor.arrow(tokenIndex)
	case current.isIdent("t") && processor.isRawString(tokenIndex+1) && processor.tokenAt(tokenIndex+1).offset == current.end():
		return tokenIndex + 1, processor.template(current, processor.tokens[tokenIndex+1])
	case current.is(token.FOR) && processor.tokenAt(tokenIndex+1).is(token.LPAREN):
		processor.forOf(tokenIndex)
	case current.is(token.LBRACK) && processor.tokenAt(tokenIndex+1).is(token.ELLIPSIS) && !processor.tokenAt(tokenIndex+2).is(token.RBRACK):
		return processor.sliceSpread(tokenIndex)
	case current.is(token.LBRACE) && processor.tokenAt(tokenIndex+1).is(token.ELLIPSIS):
		return processor.structSpread(tokenIndex)
	case current.is(token.ELLIPSIS) && processor.spreadInBraces(tokenIndex):
		return 0, &Error{current.offset, "struct spread must be the first element"}
	case (current.is(token.LBRACE) || current.is(token.LBRACK)) && processor.atStmtStart(tokenIndex):
		if next, ok, err := processor.destructure(tokenIndex, nil); ok || err != nil {
			return next, err
		}
		processor.openBracket(current, pendingTry)
	case current.is(token.LBRACE) || current.is(token.LBRACK) || current.is(token.LPAREN):
		processor.openBracket(current, pendingTry)
	case current.is(token.RPAREN) || current.is(token.RBRACK):
		processor.parens--
	case current.is(token.RBRACE):
		return processor.closeBrace(tokenIndex)
	}
	return tokenIndex, nil
}

func (processor *processor) isRawString(tokenIndex int) bool {
	current := processor.tokenAt(tokenIndex)
	return current.is(token.STRING) && strings.HasPrefix(current.text, "`")
}

func (processor *processor) openBracket(bracket lexeme, pendingTry *int) {
	if !bracket.is(token.LBRACE) {
		processor.parens++
		return
	}
	newFrame := frame{kind: frameBlock}
	if *pendingTry >= 0 {
		newFrame = frame{kind: frameTry, tryIndex: *pendingTry}
		*pendingTry = -1
	}
	processor.braceStack = append(processor.braceStack, newFrame)
}

// fresh returns an identifier that is not in the source.
func (processor *processor) fresh(base string) string {
	name := base
	for suffix := 2; processor.identifiers[name]; suffix++ {
		name = fmt.Sprintf("%s%d", base, suffix)
	}
	processor.identifiers[name] = true
	return name
}

func (processor *processor) atStmtStart(tokenIndex int) bool {
	if tokenIndex == 0 {
		return true
	}
	switch processor.tokens[tokenIndex-1].kind {
	case token.SEMICOLON, token.LBRACE, token.COLON:
		return true
	}
	return false
}

// OrigOffset maps an offset in Src to an offset in the original source. An
// offset inside generated text maps to the start of the replaced text.
func (result *Result) OrigOffset(offset int) int {
	delta := 0
	for _, change := range result.edits {
		if offset < change.newStart {
			break
		}
		if offset < change.newEnd {
			return change.origStart
		}
		delta = change.origEnd - change.newEnd
	}
	return offset + delta
}

// OrigRange maps Src[start:end] to the start of the same text in the
// original source. It reports false if generated text overlaps the range.
func (result *Result) OrigRange(start, end int) (int, bool) {
	delta := 0
	for _, change := range result.edits {
		if change.newStart >= end {
			break
		}
		if change.newEnd > start {
			return 0, false
		}
		delta = change.origEnd - change.newEnd
	}
	return start + delta, true
}
