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
	// Newlines holds the [start, end) offsets of added newlines. They keep
	// the line numbers of a construct that the output writes on one line.
	Newlines [][2]int
	// Consts holds the offsets of the names that a const declares in a
	// for-of loop, a header, or a destructuring pattern.
	Consts []int
	// TempNames holds the names of the temporary variables that the rewrite adds.
	TempNames []string
	// ForOfBlanks holds the offsets of the "_" keys that for-of loops add.
	// A range over a channel, an integer, or iter.Seq cannot have them.
	ForOfBlanks []int
	// ArrayTemps holds the temporaries of flat array patterns without a rest
	// element. If the right side is a call with more than one result, the
	// pattern takes the results instead.
	ArrayTemps []ArrayTemp
	// Params holds the offsets of the statements that declare the names of
	// destructured parameters.
	Params []int
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

const (
	frameBlock frameKind = iota
	frameTry
	frameCatch
	framePanic
)

type frame struct {
	kind     frameKind
	tryIndex int // index into Result.Tries for frameTry, frameCatch, and framePanic
}

// nesting records the bracket depth where a pending rewrite starts. The
// rewrite ends at the first terminator at the same depth.
type nesting struct {
	braces, parens int
}

func (processor *processor) nesting() nesting {
	return nesting{braces: len(processor.braceStack), parens: processor.parens}
}

// pendingCloser is a rewrite that the scan has not finished, such as an
// arrow with an expression body. text closes it at the end of its expression.
type pendingCloser struct {
	start nesting
	text  string
}

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
	pendingClosers      []pendingCloser
	// inserts maps a token index to text to add before that token.
	inserts map[int]string
	// typedArrows holds the offsets of the ")" that close arrow parameter
	// lists that already have "func" in front.
	typedArrows map[int]bool
	// bodyDestructures maps the index of a { token to the destructuring to
	// add after it.
	bodyDestructures map[int]*destructure
	// bodyStatements maps the index of the { of a function body to the
	// statements for its destructured parameters.
	bodyStatements map[int]paramStatements
	// paramRenames maps the index of the first token of a destructured
	// parameter to its rename.
	paramRenames map[int]paramRename
	// arrowStatements maps the offset of the ")" of a typed arrow parameter
	// list to the statements for its destructured parameters.
	arrowStatements map[int]paramStatements
	// genericParamOffsets holds the offsets of the parameter lists of generic
	// arrows. Their func keyword is already in the output.
	genericParamOffsets map[int]bool
}

// Process rewrites the gosling syntax in src into Go source with marker
// identifiers.
func Process(src []byte) (*Result, error) {
	return ProcessReserving(src, nil)
}

// ProcessReserving is like Process, but new temporary variables do not use
// reservedNames. Use it for names that other files of the package declare.
func ProcessReserving(src []byte, reservedNames map[string]bool) (*Result, error) {
	processor := &processor{
		src:         src,
		tokens:      tokenize(src),
		result:      &Result{},
		identifiers: map[string]bool{},
		scheduled:   map[int]string{},
		inserts:     map[int]string{},
		typedArrows: map[int]bool{},

		genericParamOffsets: map[int]bool{},

		bodyDestructures: map[int]*destructure{},
		bodyStatements:   map[int]paramStatements{},
		paramRenames:     map[int]paramRename{},
		arrowStatements:  map[int]paramStatements{},
	}
	for _, current := range processor.tokens {
		if current.kind == token.IDENT {
			processor.identifiers[current.text] = true
		}
	}
	for name := range reservedNames {
		processor.identifiers[name] = true
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
		position, kind, literal := sourceScanner.Scan()
		if kind == token.EOF {
			return lexemes
		}
		lexemes = append(lexemes, lexeme{offset: tokenFile.Offset(position), kind: kind, text: literal})
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
	processor.finishClosers(len(processor.tokens))
	return processor.finishDestructure(len(processor.tokens))
}

// rewriteInterpolation rewrites gosling syntax in src[from:to]. That range
// is an expression inside a template literal.
func (processor *processor) rewriteInterpolation(from, to int) error {
	defer processor.startScan(interpolationTokens(processor.src, from, to))()
	if err := processor.rewriteTokens(); err != nil {
		return err
	}
	processor.finishClosers(len(processor.tokens))
	return nil
}

// interpolationTokens returns the tokens of src[from:to] with offsets into
// src. It drops the automatic semicolons at the end.
func interpolationTokens(src []byte, from, to int) []lexeme {
	tokens := tokenize(src[from:to])
	for len(tokens) > 0 && tokens[len(tokens)-1].isAutoSemicolon() {
		tokens = tokens[:len(tokens)-1]
	}
	for tokenIndex := range tokens {
		tokens[tokenIndex].offset += from
	}
	return tokens
}

// startScan starts a nested scan of tokens. The returned function restores
// the outer scan. The output and the bracket depth stay shared.
func (processor *processor) startScan(tokens []lexeme) (restore func()) {
	savedTokens, savedScheduled, savedInserts := processor.tokens, processor.scheduled, processor.inserts
	savedDestructures, savedClosers := processor.pendingDestructures, processor.pendingClosers
	savedBodies, savedStatements, savedRenames := processor.bodyDestructures, processor.bodyStatements, processor.paramRenames
	processor.tokens, processor.scheduled, processor.inserts = tokens, map[int]string{}, map[int]string{}
	processor.pendingDestructures, processor.pendingClosers = nil, nil
	processor.bodyDestructures, processor.bodyStatements, processor.paramRenames = map[int]*destructure{}, map[int]paramStatements{}, map[int]paramRename{}
	return func() {
		processor.tokens, processor.scheduled, processor.inserts = savedTokens, savedScheduled, savedInserts
		processor.pendingDestructures, processor.pendingClosers = savedDestructures, savedClosers
		processor.bodyDestructures, processor.bodyStatements, processor.paramRenames = savedBodies, savedStatements, savedRenames
	}
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
		if statement, ok := processor.bodyDestructures[tokenIndex]; ok {
			insertedAt := processor.replace(current.end(), current.end(), statement.text)
			processor.recordConsts(insertedAt+len(" "), statement.constBindings)
			delete(processor.bodyDestructures, tokenIndex)
		}
		if statements, ok := processor.bodyStatements[tokenIndex]; ok {
			processor.insertParamStatements(current.end(), statements)
			delete(processor.bodyStatements, tokenIndex)
		}
		processor.finishClosers(tokenIndex)
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
	if lastIndex, ok := processor.renameParam(tokenIndex); ok {
		return lastIndex, nil
	}
	switch {
	case current.isIdent("try") && processor.tokenAt(tokenIndex+1).is(token.LBRACE) && processor.atStmtStart(tokenIndex):
		processor.result.Tries = append(processor.result.Tries, Try{If: processor.replace(current.offset, current.end(), "if true")})
		*pendingTry = len(processor.result.Tries) - 1
	case processor.isThrow(tokenIndex):
		return tokenIndex, processor.rewriteThrow(tokenIndex)
	case current.isIdent("try") && !processor.tokenAt(tokenIndex+1).is(token.LBRACE):
		return tokenIndex, processor.rewriteTryExpression(tokenIndex)
	case current.is(token.ILLEGAL) && current.text == "?":
		return processor.questionMark(tokenIndex)
	case current.is(token.TYPE):
		return processor.union(tokenIndex)
	case (current.isIdent("let") || current.is(token.CONST)) && (processor.atStmtStart(tokenIndex) || processor.inHeader(tokenIndex)):
		return processor.declaration(tokenIndex)
	case current.is(token.PERIOD) && processor.tokenAt(tokenIndex+1).is(token.MAP) && processor.touchesNext(tokenIndex):
		processor.replace(processor.tokenAt(tokenIndex+1).offset, processor.tokenAt(tokenIndex+1).end(), MapMarker)
		return tokenIndex + 1, nil
	case current.is(token.ASSIGN) && processor.tokenAt(tokenIndex+1).is(token.GTR) && processor.touchesNext(tokenIndex):
		return processor.arrow(tokenIndex)
	case current.isIdent("t") && processor.isRawString(tokenIndex+1) && processor.touchesNext(tokenIndex):
		return tokenIndex + 1, processor.template(current, processor.tokens[tokenIndex+1])
	case current.is(token.FOR) && processor.tokenAt(tokenIndex+1).is(token.LPAREN):
		return processor.forOf(tokenIndex)
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
		if current.is(token.LBRACK) && processor.hasSpreadElement(tokenIndex) {
			return processor.sliceSpread(tokenIndex)
		}
		processor.openBracket(current, pendingTry)
	case current.is(token.FUNC):
		if err := processor.signature(tokenIndex); err != nil {
			return 0, err
		}
		return processor.rewriteFuncParams(tokenIndex)
	case current.is(token.INTERFACE) && processor.tokenAt(tokenIndex+1).is(token.LBRACE):
		return tokenIndex, processor.interfaceMethods(tokenIndex)
	case current.is(token.LPAREN):
		processor.openBracket(current, pendingTry)
		return tokenIndex, processor.typedArrow(tokenIndex)
	case current.is(token.LBRACK) && processor.hasSpreadElement(tokenIndex):
		return processor.sliceSpread(tokenIndex)
	case current.is(token.LBRACE) || current.is(token.LBRACK):
		processor.openBracket(current, pendingTry)
	case current.is(token.RPAREN) || current.is(token.RBRACK):
		processor.parens--
	case current.is(token.RBRACE):
		return processor.closeBrace(tokenIndex)
	}
	return tokenIndex, nil
}

// touchesNext reports whether the next token starts where the token at
// tokenIndex ends, with no space between them.
func (processor *processor) touchesNext(tokenIndex int) bool {
	return processor.tokenAt(tokenIndex+1).offset == processor.tokenAt(tokenIndex).end()
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

// finishClosers closes the pending rewrites whose expression ends at token
// tokenIndex.
func (processor *processor) finishClosers(tokenIndex int) {
	for len(processor.pendingClosers) > 0 {
		pending := processor.pendingClosers[len(processor.pendingClosers)-1]
		if processor.nesting() != pending.start {
			return
		}
		switch processor.tokenAt(tokenIndex).kind {
		case token.COMMA, token.RPAREN, token.RBRACK, token.RBRACE, token.SEMICOLON, token.COLON, token.EOF:
		default:
			return
		}
		processor.pendingClosers = processor.pendingClosers[:len(processor.pendingClosers)-1]
		end := processor.tokenAt(tokenIndex - 1).end()
		processor.replace(end, end, pending.text)
	}
}
