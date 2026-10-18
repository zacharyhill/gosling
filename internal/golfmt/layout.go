package golfmt

import (
	"bytes"
	"go/token"
	"strings"

	"github.com/zacharyhill/gosling/internal/preprocess"
)

// maxResyncDistance limits how far token alignment looks ahead when the
// tokens before and after gofmt stop matching.
const maxResyncDistance = 64

// tabWidth is the tab width that gofmt uses to align columns.
const tabWidth = 8

// gap is the space before a token, or after the last token.
type gap struct {
	start, end  int
	text        string
	isFormatted bool
	// nextToken is the offset in the gofmt output of the token after the
	// gap. It is valid only if isFormatted is true.
	nextToken int
	// indent is the gofmt indentation of the token after the gap, if that
	// token starts a line in the gofmt output.
	indent    string
	hasIndent bool
	// generatedDepth is how many more brackets are open before the token
	// after the gap in the preprocessed code than in the .gol file.
	generatedDepth    int
	hasGeneratedDepth bool
}

// layout holds the tokens of a .gol file and the new text of each gap.
// gaps[i] comes before tokens[i]. The last gap ends the file.
type layout struct {
	src         []byte
	tokens      []sourceToken
	gaps        []gap
	gapsByStart map[int]int
	gapsByEnd   map[int]int
}

func newLayout(src []byte) *layout {
	tokens := scanTokens(src, false)
	layout := &layout{src: src, tokens: tokens, gapsByStart: map[int]int{}, gapsByEnd: map[int]int{}}
	previousEnd := 0
	for _, current := range append(tokens, sourceToken{start: len(src), end: len(src)}) {
		layout.gapsByStart[previousEnd] = len(layout.gaps)
		layout.gapsByEnd[current.start] = len(layout.gaps)
		layout.gaps = append(layout.gaps, gap{start: previousEnd, end: current.start, text: string(src[previousEnd:current.start])})
		previousEnd = current.end
	}
	return layout
}

// gofmtResult pairs the tokens of the preprocessed code with the tokens of
// its gofmt output.
type gofmtResult struct {
	preprocessed     *preprocess.Result
	processedTokens  []sourceToken
	formatted        []byte
	formattedTokens  []sourceToken
	formattedIndexes []int // the formatted token of each processed token
}

func newGofmtResult(preprocessed *preprocess.Result, formatted []byte) gofmtResult {
	processedTokens, formattedTokens := scanTokens(preprocessed.Src, false), scanTokens(formatted, false)
	return gofmtResult{
		preprocessed:     preprocessed,
		processedTokens:  processedTokens,
		formatted:        formatted,
		formattedTokens:  formattedTokens,
		formattedIndexes: alignTokens(processedTokens, formattedTokens),
	}
}

// copyGaps sets the text of each gap that gofmt formats. A gap gets the
// gofmt space if the tokens on both sides map to adjacent tokens in the
// gofmt output. A gap must keep whether it has a newline, because a newline
// can end a statement. The tokens on both sides must be the same as in the
// .gol file. Space next to generated text, such as the "{" of a struct
// spread, does not fit the gosling syntax that it replaces.
func (layout *layout) copyGaps(result gofmtResult) {
	layout.copyIndents(result)
	processedTokens, formattedTokens := result.processedTokens, result.formattedTokens
	for left := -1; left < len(processedTokens); left++ {
		formattedLeft := boundaryIndex(result.formattedIndexes, left, len(formattedTokens))
		formattedRight := boundaryIndex(result.formattedIndexes, left+1, len(formattedTokens))
		if formattedLeft == unaligned || formattedRight != formattedLeft+1 {
			continue
		}
		processedStart, processedEnd := tokenEndAt(processedTokens, left), tokenStartAt(processedTokens, left+1, len(result.preprocessed.Src))
		sourceStart, ok := result.preprocessed.OrigRange(processedStart, processedEnd)
		if !ok {
			continue
		}
		gapIndex, ok := layout.gapsByStart[sourceStart]
		if !ok || layout.gaps[gapIndex].end != sourceStart+processedEnd-processedStart {
			continue
		}
		if !layout.hasSameNeighbors(gapIndex, tokenAt(processedTokens, left), tokenAt(processedTokens, left+1)) {
			continue
		}
		formattedStart, formattedEnd := tokenEndAt(formattedTokens, formattedLeft), tokenStartAt(formattedTokens, formattedRight, len(result.formatted))
		formattedText := string(result.formatted[formattedStart:formattedEnd])
		isEdge := gapIndex == 0 || gapIndex == len(layout.gaps)-1
		if !isEdge && hasNewline(formattedText) != hasNewline(layout.gaps[gapIndex].text) {
			continue
		}
		layout.gaps[gapIndex].text = formattedText
		layout.gaps[gapIndex].isFormatted = true
		layout.gaps[gapIndex].nextToken = formattedEnd
	}
}

const unaligned = -2

func tokenAt(tokens []sourceToken, tokenIndex int) sourceToken {
	if tokenIndex < 0 || tokenIndex >= len(tokens) {
		return sourceToken{}
	}
	return tokens[tokenIndex]
}

// hasSameNeighbors reports whether the tokens around a gap have the text of the
// preprocessed tokens left and right.
func (layout *layout) hasSameNeighbors(gapIndex int, left, right sourceToken) bool {
	sourceLeft, sourceRight := tokenAt(layout.tokens, gapIndex-1), tokenAt(layout.tokens, gapIndex)
	return sourceLeft.text == left.text && sourceRight.text == right.text
}

// copyIndents records the gofmt indentation of each token that starts a
// line in the gofmt output. A gap that gofmt does not format can still use
// it. A generated token, such as the "if" that replaces "try", gives the
// indentation of the gosling syntax that it replaces.
func (layout *layout) copyIndents(result gofmtResult) {
	sourceDepths := layout.computeSourceDepths()
	seen := map[int]bool{}
	processedDepth := 0
	for processedIndex, formattedIndex := range result.formattedIndexes {
		processedDepthBefore := processedDepth
		processedDepth += bracketChange(result.processedTokens[processedIndex].kind)
		// An empty range maps to the text before a removal, such as the
		// "let" that "let x = 1" loses.
		tokenStart := result.processedTokens[processedIndex].start
		sourceStart, ok := result.preprocessed.OrigRange(tokenStart, tokenStart)
		if !ok {
			continue
		}
		gapIndex, ok := layout.gapsByEnd[sourceStart]
		if !ok || seen[gapIndex] {
			continue
		}
		seen[gapIndex] = true
		layout.gaps[gapIndex].generatedDepth = processedDepthBefore - sourceDepths[gapIndex]
		layout.gaps[gapIndex].hasGeneratedDepth = true
		if formattedIndex < 0 {
			continue
		}
		formattedStart := result.formattedTokens[formattedIndex].start
		lineStart := bytes.LastIndexByte(result.formatted[:formattedStart], '\n') + 1
		indent := string(result.formatted[lineStart:formattedStart])
		if strings.Trim(indent, " \t") == "" {
			layout.gaps[gapIndex].indent, layout.gaps[gapIndex].hasIndent = indent, true
		}
	}
}

// computeSourceDepths returns the bracket depth before each token.
func (layout *layout) computeSourceDepths() []int {
	depths := make([]int, len(layout.gaps))
	depth := 0
	for tokenIndex, current := range layout.tokens {
		depths[tokenIndex] = depth
		depth += bracketChange(current.kind)
	}
	depths[len(layout.tokens)] = depth
	return depths
}

// boundaryIndex returns the gofmt index of the token at tokenIndex. The
// start of a file is token -1, and the end is the token after the last one.
func boundaryIndex(indexes []int, tokenIndex, formattedCount int) int {
	switch {
	case tokenIndex < 0:
		return -1
	case tokenIndex >= len(indexes):
		return formattedCount
	}
	return indexes[tokenIndex]
}

func tokenEndAt(tokens []sourceToken, tokenIndex int) int {
	if tokenIndex < 0 {
		return 0
	}
	return tokens[tokenIndex].end
}

func tokenStartAt(tokens []sourceToken, tokenIndex, length int) int {
	if tokenIndex >= len(tokens) {
		return length
	}
	return tokens[tokenIndex].start
}

func hasNewline(text string) bool {
	return strings.IndexByte(text, '\n') >= 0
}

// alignTokens pairs equal tokens of before and after in order. It returns
// the index in after of each token of before, or unaligned. gofmt removes
// some tokens, such as semicolons and parentheses, and sorts imports. So
// after a mismatch it looks ahead for the next match.
func alignTokens(before, after []sourceToken) []int {
	indexes := make([]int, len(before))
	for beforeIndex := range indexes {
		indexes[beforeIndex] = unaligned
	}
	beforeIndex, afterIndex := 0, 0
	for beforeIndex < len(before) && afterIndex < len(after) {
		if before[beforeIndex].text == after[afterIndex].text {
			indexes[beforeIndex] = afterIndex
			beforeIndex++
			afterIndex++
			continue
		}
		beforeSkip, afterSkip := resync(before[beforeIndex:], after[afterIndex:])
		beforeIndex += beforeSkip
		afterIndex += afterSkip
	}
	return indexes
}

// resync returns how many tokens to skip in each sequence to reach the
// nearest pair of equal tokens.
func resync(before, after []sourceToken) (int, int) {
	for distance := 1; distance <= maxResyncDistance; distance++ {
		for beforeSkip := 0; beforeSkip <= distance; beforeSkip++ {
			afterSkip := distance - beforeSkip
			if beforeSkip < len(before) && afterSkip < len(after) && before[beforeSkip].text == after[afterSkip].text {
				return beforeSkip, afterSkip
			}
		}
	}
	return 1, 1
}

// maxNewlines is the most newlines that gofmt keeps between two tokens. It
// keeps one blank line.
const maxNewlines = 2

// cleanKeptGaps tidies the gaps that gofmt does not format. These gaps are
// next to gosling syntax, or next to tokens that gofmt changes, such as
// sorted imports. Lines lose trailing space and get the gofmt indentation.
// Space inside a line becomes one space, except before a comment, where it
// can align columns. Some gosling syntax also gets a space where it has
// none, such as in "}catch(e){".
func (layout *layout) cleanKeptGaps() {
	tracker := indentTracker{lastIndentByBracket: map[int]string{}}
	for gapIndex := range layout.gaps {
		current := &layout.gaps[gapIndex]
		isCloserLine := gapIndex < len(layout.tokens) && isCloser(layout.tokens[gapIndex].kind)
		switch {
		case hasNewline(current.text):
			current.text = tracker.buildLineStart(*current, isCloserLine)
			tracker.startLine(current.text, isCloserLine)
		case current.isFormatted:
		case current.text != "" && !layout.isBeforeComment(gapIndex):
			current.text = " "
		case current.text == "" && layout.needsSpace(gapIndex):
			current.text = " "
		}
		if gapIndex < len(layout.tokens) {
			tracker.addToken(layout.tokens[gapIndex].kind, gapIndex, *current)
		}
	}
}

// openBracket is a bracket that a line before the current line opens.
type openBracket struct {
	tokenIndex int
	line       lineState
}

// lineState describes a line of the formatted file.
type lineState struct {
	indent            string
	generatedDepth    int
	hasGeneratedDepth bool
}

// indentTracker follows the open brackets and the indentation of lines
// while cleanKeptGaps walks the gaps.
type indentTracker struct {
	openBrackets        []openBracket
	line                lineState
	lastIndentByBracket map[int]string
}

// buildLineStart returns the text of a gap that starts a line. Generated
// brackets, such as those of a struct spread, can make the gofmt
// indentation deeper or shallower than the .gol brackets. Then the line
// gets one tab more than the line of its innermost open bracket.
func (tracker *indentTracker) buildLineStart(current gap, isCloserLine bool) string {
	newlines := strings.Repeat("\n", min(strings.Count(current.text, "\n"), maxNewlines))
	switch {
	case !tracker.canUseGofmtIndent(current, isCloserLine):
		return newlines + tracker.bracketIndent(isCloserLine)
	case current.isFormatted:
		return current.text
	case current.hasIndent:
		return newlines + current.indent
	case isCloserLine:
		return newlines + tracker.bracketIndent(isCloserLine)
	}
	if indent, ok := tracker.lastIndentByBracket[tracker.innermostBracket()]; ok {
		return newlines + indent
	}
	return newlines + tracker.bracketIndent(isCloserLine)
}

// canUseGofmtIndent reports whether the line after a gap has as many
// generated brackets open as the line of its innermost open bracket.
func (tracker *indentTracker) canUseGofmtIndent(current gap, isCloserLine bool) bool {
	reference := lineState{hasGeneratedDepth: true}
	if count := len(tracker.openBrackets); count > 0 {
		reference = tracker.openBrackets[count-1].line
	}
	if !current.hasGeneratedDepth || !reference.hasGeneratedDepth {
		return true
	}
	return current.generatedDepth == reference.generatedDepth
}

// bracketIndent returns the indentation of the line of the innermost open
// bracket, plus a tab unless the line starts with the closing bracket.
func (tracker *indentTracker) bracketIndent(isCloserLine bool) string {
	count := len(tracker.openBrackets)
	if count == 0 {
		return ""
	}
	indent := tracker.openBrackets[count-1].line.indent
	if isCloserLine {
		return indent
	}
	return indent + "\t"
}

// innermostBracket returns the token index of the innermost open bracket,
// or -1 outside all brackets.
func (tracker *indentTracker) innermostBracket() int {
	if count := len(tracker.openBrackets); count > 0 {
		return tracker.openBrackets[count-1].tokenIndex
	}
	return -1
}

func (tracker *indentTracker) startLine(text string, isCloserLine bool) {
	tracker.line = lineState{indent: text[strings.LastIndexByte(text, '\n')+1:]}
	if !isCloserLine {
		tracker.lastIndentByBracket[tracker.innermostBracket()] = tracker.line.indent
	}
}

// addToken records the generated depth of the line and follows the
// brackets. The line takes the generated depth of its first token that has
// one.
func (tracker *indentTracker) addToken(kind token.Token, tokenIndex int, before gap) {
	if !tracker.line.hasGeneratedDepth && before.hasGeneratedDepth {
		tracker.line.generatedDepth, tracker.line.hasGeneratedDepth = before.generatedDepth, true
	}
	switch bracketChange(kind) {
	case 1:
		tracker.openBrackets = append(tracker.openBrackets, openBracket{tokenIndex: tokenIndex, line: tracker.line})
	case -1:
		if count := len(tracker.openBrackets); count > 0 {
			tracker.openBrackets = tracker.openBrackets[:count-1]
		}
	}
}

func isCloser(kind token.Token) bool {
	return kind == token.RPAREN || kind == token.RBRACK || kind == token.RBRACE
}

func bracketChange(kind token.Token) int {
	switch kind {
	case token.LPAREN, token.LBRACK, token.LBRACE:
		return 1
	case token.RPAREN, token.RBRACK, token.RBRACE:
		return -1
	}
	return 0
}

// spacedPairs lists the gosling keywords and the tokens next to them that
// take a space between them.
var spacedPairs = map[[2]string]bool{
	{"try", "{"}:     true,
	{"}", "catch"}:   true,
	{"catch", "("}:   true,
	{"catch", "{"}:   true,
	{")", "{"}:       true,
	{"}", "finally"}: true,
	{"finally", "{"}: true,
}

// needsSpace reports whether an empty gap gets a space. Each side of the
// operators "=>" and "??" also gets one.
func (layout *layout) needsSpace(gapIndex int) bool {
	if gapIndex == 0 || gapIndex >= len(layout.tokens) {
		return false
	}
	left, right := layout.tokens[gapIndex-1], layout.tokens[gapIndex]
	if spacedPairs[[2]string{left.text, right.text}] {
		return true
	}
	if layout.isInNullishAssign(gapIndex) {
		return false
	}
	return layout.startsOperator(gapIndex) || layout.endsOperator(gapIndex-1)
}

// isInNullishAssign reports whether the gap is between the "??" and the "="
// of a "??=" operator.
func (layout *layout) isInNullishAssign(gapIndex int) bool {
	left, right := layout.tokens[gapIndex-1], layout.tokens[gapIndex]
	return right.text == "=" && left.end == right.start && layout.endsOperator(gapIndex-1)
}

// startsOperator reports whether the token at tokenIndex starts "=>" or "??".
func (layout *layout) startsOperator(tokenIndex int) bool {
	first, second := tokenAt(layout.tokens, tokenIndex), tokenAt(layout.tokens, tokenIndex+1)
	return first.end == second.start && isTwoTokenOperator(first.text, second.text)
}

// endsOperator reports whether the token at tokenIndex ends "=>" or "??".
func (layout *layout) endsOperator(tokenIndex int) bool {
	first, second := tokenAt(layout.tokens, tokenIndex-1), tokenAt(layout.tokens, tokenIndex)
	return first.end == second.start && isTwoTokenOperator(first.text, second.text)
}

func isTwoTokenOperator(first, second string) bool {
	return (first == "=" && second == ">") || (first == "?" && second == "?")
}

func (layout *layout) isBeforeComment(gapIndex int) bool {
	return gapIndex < len(layout.tokens) && strings.HasPrefix(layout.tokens[gapIndex].text, "/")
}

// alignComments aligns the comments at the ends of lines. gofmt aligns them
// in the preprocessed code, where a line can have a different width. Each
// group of comments that gofmt aligns gets the column after its widest line.
func (layout *layout) alignComments(formatted []byte) {
	columns := layout.computeGapColumns()
	var group []int
	previousLine, previousColumn := -1, -1
	for gapIndex, current := range layout.gaps {
		if !layout.isTrailingComment(gapIndex) {
			continue
		}
		line, column := lineAndColumn(formatted, current.nextToken)
		if line != previousLine+1 || column != previousColumn {
			layout.alignGroup(group, columns)
			group = nil
		}
		group = append(group, gapIndex)
		previousLine, previousColumn = line, column
	}
	layout.alignGroup(group, columns)
}

// isTrailingComment reports whether gofmt formats the gap before a comment
// that ends its line.
func (layout *layout) isTrailingComment(gapIndex int) bool {
	current := layout.gaps[gapIndex]
	if gapIndex == 0 || !current.isFormatted || hasNewline(current.text) || !layout.isBeforeComment(gapIndex) {
		return false
	}
	next := layout.gaps[gapIndex+1]
	return gapIndex+1 == len(layout.gaps)-1 || hasNewline(next.text)
}

func (layout *layout) alignGroup(group []int, columns []int) {
	widest := 0
	for _, gapIndex := range group {
		widest = max(widest, columns[gapIndex])
	}
	for _, gapIndex := range group {
		layout.gaps[gapIndex].text = strings.Repeat(" ", widest+1-columns[gapIndex])
	}
}

// computeGapColumns returns the column where each gap starts in the formatted file.
func (layout *layout) computeGapColumns() []int {
	columns := make([]int, len(layout.gaps))
	column := 0
	for gapIndex, current := range layout.gaps {
		columns[gapIndex] = column
		column = advanceColumn(column, current.text)
		if gapIndex < len(layout.tokens) {
			column = advanceColumn(column, layout.tokens[gapIndex].text)
		}
	}
	return columns
}

func advanceColumn(column int, text string) int {
	for _, char := range text {
		switch char {
		case '\n':
			column = 0
		case '\t':
			column = (column/tabWidth + 1) * tabWidth
		default:
			column++
		}
	}
	return column
}

// lineAndColumn returns the line and column of offset in text. Lines count
// from 0.
func lineAndColumn(text []byte, offset int) (int, int) {
	lineStart := bytes.LastIndexByte(text[:offset], '\n') + 1
	return bytes.Count(text[:offset], []byte("\n")), advanceColumn(0, string(text[lineStart:offset]))
}

// buildEdits returns an edit for each changed gap. An edit covers only the
// changed part of its gap.
func (layout *layout) buildEdits() []Edit {
	var edits []Edit
	for _, current := range layout.gaps {
		old := string(layout.src[current.start:current.end])
		if old == current.text {
			continue
		}
		prefix := commonPrefix(old, current.text)
		suffix := commonSuffix(old[prefix:], current.text[prefix:])
		edits = append(edits, Edit{Start: current.start + prefix, End: current.end - suffix, Text: current.text[prefix : len(current.text)-suffix]})
	}
	return edits
}

func commonPrefix(left, right string) int {
	length := 0
	for length < len(left) && length < len(right) && left[length] == right[length] {
		length++
	}
	return length
}

func commonSuffix(left, right string) int {
	length := 0
	for length < len(left) && length < len(right) && left[len(left)-1-length] == right[len(right)-1-length] {
		length++
	}
	return length
}
