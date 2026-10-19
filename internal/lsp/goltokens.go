package lsp

import (
	"go/scanner"
	"go/token"
)

// golToken is a token of a .gol file. The scanner sees gosling syntax as Go
// tokens, and "?" as an illegal token.
type golToken struct {
	start, end int
	kind       token.Token
	text       string
}

// scanGolTokens returns the tokens of a .gol file, without comments and
// automatic semicolons.
func scanGolTokens(text []byte) []golToken {
	tokenFile := token.NewFileSet().AddFile("", -1, len(text))
	var golScanner scanner.Scanner
	golScanner.Init(tokenFile, text, func(token.Position, string) {}, 0)
	var tokens []golToken
	for {
		tokenPosition, kind, literal := golScanner.Scan()
		if kind == token.EOF {
			return tokens
		}
		if kind == token.SEMICOLON && literal != ";" {
			continue
		}
		if literal == "" {
			literal = kind.String()
		}
		start := tokenFile.Offset(tokenPosition)
		tokens = append(tokens, golToken{start: start, end: start + len(literal), kind: kind, text: literal})
	}
}
