package transpile

import (
	"fmt"
	"go/scanner"
	"go/token"
	"regexp"
	"strconv"
)

var lineDirectiveRE = regexp.MustCompile(`^/\*line (.+):(\d+)\*/$`)

// restoreDirectiveLines fixes the /*line*/ comments that gofmt moved. gofmt
// can move the token after a directive to the next line. It can also move a
// comma in front of a directive. In unformatted, each directive is directly before
// its token, so the token lines are correct. Each directive in formatted gets
// the line that gives the next token its line from unformatted.
func restoreDirectiveLines(unformatted, formatted []byte) []byte {
	wantLines := scanSource(unformatted).lines(true)
	formattedSource := scanSource(formatted)
	gotLines := formattedSource.lines(false)
	if len(gotLines) != len(wantLines) {
		return formatted // gofmt changed the tokens, so the token lists do not match
	}
	var rewritten []byte
	copiedUpTo := 0
	for _, directive := range formattedSource.directives {
		if directive.nextToken >= len(gotLines) {
			continue
		}
		directiveLine := formattedSource.tokenFile.PositionFor(directive.position, false).Line
		correctLine := wantLines[directive.nextToken] - (gotLines[directive.nextToken] - directiveLine)
		if correctLine < 1 || correctLine == directive.line {
			continue
		}
		directiveStart := formattedSource.tokenFile.Offset(directive.position)
		rewritten = append(rewritten, formatted[copiedUpTo:directiveStart]...)
		rewritten = fmt.Appendf(rewritten, "/*line %s:%d*/", directive.file, correctLine)
		copiedUpTo = directiveStart + directive.length
	}
	return append(rewritten, formatted[copiedUpTo:]...)
}

// lineDirective is a /*line*/ comment. nextToken is the index of the token
// after it.
type lineDirective struct {
	position  token.Pos
	length    int
	file      string
	line      int
	nextToken int
}

// scannedSource holds the tokens that gofmt keeps, and the line directives.
type scannedSource struct {
	tokenFile  *token.File
	tokens     []token.Pos
	directives []lineDirective
}

// newScanner returns a scanner for src and the token file of its positions.
// The scanner ignores errors.
func newScanner(src []byte, mode scanner.Mode) (*scanner.Scanner, *token.File) {
	tokenFile := token.NewFileSet().AddFile("", -1, len(src))
	sourceScanner := &scanner.Scanner{}
	sourceScanner.Init(tokenFile, src, nil, mode)
	return sourceScanner, tokenFile
}

func scanSource(src []byte) scannedSource {
	sourceScanner, tokenFile := newScanner(src, scanner.ScanComments)
	scanned := scannedSource{tokenFile: tokenFile}
	for {
		position, kind, literal := sourceScanner.Scan()
		switch kind {
		case token.EOF:
			return scanned
		case token.SEMICOLON, token.LPAREN, token.RPAREN:
			// gofmt adds and removes semicolons and parentheses, so the
			// scan skips them.
		case token.COMMENT:
			scanned.addDirective(position, literal)
		default:
			scanned.tokens = append(scanned.tokens, position)
		}
	}
}

// addDirective records comment if it is a line directive.
func (scanned *scannedSource) addDirective(position token.Pos, comment string) {
	match := lineDirectiveRE.FindStringSubmatch(comment)
	if match == nil {
		return
	}
	line, err := strconv.Atoi(match[2])
	if err != nil {
		return
	}
	scanned.directives = append(scanned.directives, lineDirective{position: position, length: len(comment), file: match[1], line: line, nextToken: len(scanned.tokens)})
}

// lines returns the line of each token. With followDirectives, the lines
// come from the /*line*/ comments, not from the file.
func (scanned scannedSource) lines(followDirectives bool) []int {
	tokenLines := make([]int, len(scanned.tokens))
	for tokenIndex, position := range scanned.tokens {
		tokenLines[tokenIndex] = scanned.tokenFile.PositionFor(position, followDirectives).Line
	}
	return tokenLines
}
