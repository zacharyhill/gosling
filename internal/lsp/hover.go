package lsp

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
)

// maxSnippetLines limits the generated code in a hover.
const maxSnippetLines = 30

// constructDescriptions describe the gosling syntax that has no Go token.
var constructDescriptions = map[string]string{
	"try":     "gosling checks each call in a try block whose last result is `error`. A failure goes to the catch block.",
	"catch":   "The catch block runs when a checked call in the try block fails.",
	"finally": "The finally block runs after the try or catch block, on each exit except a panic.",
	"t":       "A template literal. gosling changes it to a `fmt.Sprintf` call.",
	"??":      "`x ?? d` gives `d` if `x` is nil.",
	"?.":      "`a?.b` reads `b` only if `a` is not nil. Otherwise the chain gives the zero value.",
}

// construct is gosling syntax at a hover position. start and end hold the
// .gol text whose generated code the hover shows.
type construct struct {
	keyword    string
	start, end int
	// catchVariable is the name of a catch variable, if the hover is on one.
	catchVariable string
}

// buildGeneratedHover describes the gosling syntax at a position that has no
// copied token, such as try or ??. The hover shows the generated Go code.
// It returns nil for other positions.
func (server *server) buildGeneratedHover(file *generatedFile, golPosition position) any {
	currentText := server.currentGolText(file)
	offset, ok := offsetAt(currentText, golPosition)
	if !ok {
		return nil
	}
	if !bytes.Equal(currentText, file.golText) {
		if offset, ok = diffText(file.golText, currentText).toOld(offset); !ok {
			return nil
		}
	}
	found, ok := findConstructAt(scanGolTokens(file.golText), offset)
	if !ok {
		return nil
	}
	return map[string]any{"contents": map[string]any{"kind": "markdown", "value": describeConstruct(file, found)}}
}

func describeConstruct(file *generatedFile, found construct) string {
	if found.catchVariable != "" {
		return "```go\nvar " + found.catchVariable + " error\n```\n\nThe error of the call that failed in the try block."
	}
	text := "`" + found.keyword + "` (gosling)\n\n" + constructDescriptions[found.keyword]
	if snippet := extractGeneratedSnippet(file, found); snippet != "" {
		text += "\n\nGenerated Go:\n\n```go\n" + snippet + "\n```"
	}
	return text
}

// findConstructAt finds the gosling syntax that contains offset.
func findConstructAt(tokens []golToken, offset int) (construct, bool) {
	tokenIndex := findTokenIndex(tokens, offset)
	if tokenIndex < 0 {
		return construct{}, false
	}
	tokenAt := func(index int) golToken {
		if index < 0 || index >= len(tokens) {
			return golToken{}
		}
		return tokens[index]
	}
	touchesNext := func(index int) bool { return tokenAt(index).end == tokenAt(index+1).start }
	current := tokens[tokenIndex]
	switch {
	case current.text == "try" || current.text == "catch" || current.text == "finally":
		return findTryStatement(tokens, tokenIndex)
	case current.kind == token.IDENT && tokenAt(tokenIndex-1).text == "(" && tokenAt(tokenIndex-2).text == "catch" && tokenAt(tokenIndex+1).text == ")":
		return construct{catchVariable: current.text}, true
	case current.text == "t" && strings.HasPrefix(tokenAt(tokenIndex+1).text, "`") && touchesNext(tokenIndex):
		return construct{keyword: "t", start: current.start, end: tokenAt(tokenIndex + 1).end}, true
	case strings.HasPrefix(current.text, "`") && tokenAt(tokenIndex-1).text == "t" && touchesNext(tokenIndex-1):
		return construct{keyword: "t", start: tokenAt(tokenIndex - 1).start, end: current.end}, true
	case current.text == "?" && tokenAt(tokenIndex+1).text == "?" && touchesNext(tokenIndex),
		current.text == "?" && tokenAt(tokenIndex-1).text == "?" && touchesNext(tokenIndex-1):
		return construct{keyword: "??", start: current.start, end: current.end}, true
	case current.text == "?" && tokenAt(tokenIndex+1).text == "." && touchesNext(tokenIndex),
		current.text == "." && tokenAt(tokenIndex-1).text == "?" && touchesNext(tokenIndex-1):
		return construct{keyword: "?.", start: current.start, end: current.end}, true
	}
	return construct{}, false
}

// findTokenIndex returns the index of the token that contains offset, or -1.
// A token that starts at offset wins over a token that ends there.
func findTokenIndex(tokens []golToken, offset int) int {
	tokenIndex := -1
	for candidateIndex, candidate := range tokens {
		if candidate.start <= offset && offset <= candidate.end {
			tokenIndex = candidateIndex
			if offset < candidate.end {
				break
			}
		}
	}
	return tokenIndex
}

// findTryStatement returns the try statement of the try, catch, or finally
// keyword at tokenIndex.
func findTryStatement(tokens []golToken, tokenIndex int) (construct, bool) {
	tryIndex, ok := findTry(tokens, tokenIndex)
	if !ok {
		return construct{}, false
	}
	blockEnd, ok := findClosingBrace(tokens, tryIndex+1)
	if !ok {
		return construct{}, false
	}
	next := blockEnd + 1
	if next < len(tokens) && tokens[next].text == "catch" {
		next++
		if next+2 < len(tokens) && tokens[next].text == "(" && tokens[next+2].text == ")" {
			next += 3
		}
		if blockEnd, ok = findClosingBrace(tokens, next); !ok {
			return construct{}, false
		}
		next = blockEnd + 1
	}
	if next+1 < len(tokens) && tokens[next].text == "finally" {
		if blockEnd, ok = findClosingBrace(tokens, next+1); !ok {
			return construct{}, false
		}
	}
	return construct{keyword: tokens[tokenIndex].text, start: tokens[tryIndex].start, end: tokens[blockEnd].end}, true
}

// findTry walks back from a try, catch, or finally keyword to its try.
func findTry(tokens []golToken, keywordIndex int) (int, bool) {
	for index := keywordIndex; index >= 0; {
		switch tokens[index].text {
		case "try":
			return index, index+1 < len(tokens) && tokens[index+1].text == "{"
		case "catch", "finally":
			if index == 0 || tokens[index-1].text != "}" {
				return 0, false
			}
			blockStart, ok := findOpeningBrace(tokens, index-1)
			if !ok || blockStart == 0 {
				return 0, false
			}
			index = blockStart - 1
			if index >= 3 && tokens[index].text == ")" && tokens[index-2].text == "(" {
				index -= 3
			}
		default:
			return 0, false
		}
	}
	return 0, false
}

// findClosingBrace returns the index of the "}" that closes the "{" at openIndex.
func findClosingBrace(tokens []golToken, openIndex int) (int, bool) {
	if openIndex >= len(tokens) || tokens[openIndex].text != "{" {
		return 0, false
	}
	depth := 0
	for index := openIndex; index < len(tokens); index++ {
		switch tokens[index].text {
		case "{":
			depth++
		case "}":
			depth--
			if depth == 0 {
				return index, true
			}
		}
	}
	return 0, false
}

// findOpeningBrace returns the index of the "{" that the "}" at closeIndex
// closes.
func findOpeningBrace(tokens []golToken, closeIndex int) (int, bool) {
	depth := 0
	for index := closeIndex; index >= 0; index-- {
		switch tokens[index].text {
		case "}":
			depth++
		case "{":
			depth--
			if depth == 0 {
				return index, true
			}
		}
	}
	return 0, false
}

// extractGeneratedSnippet returns the generated code of a construct, or ""
// if the generated file has no code that matches.
func extractGeneratedSnippet(file *generatedFile, found construct) string {
	offsets := findGeneratedOffsets(file, found)
	if len(offsets) == 0 {
		return ""
	}
	fileSet := token.NewFileSet()
	astFile, err := parser.ParseFile(fileSet, "", file.goText, parser.SkipObjectResolution)
	if err != nil {
		return ""
	}
	tokenFile := fileSet.File(astFile.Pos())
	path, _ := astutil.PathEnclosingInterval(astFile, tokenFile.Pos(offsets[0]), tokenFile.Pos(offsets[len(offsets)-1]))
	node := findGeneratedNode(path, found.keyword)
	if node == nil {
		return ""
	}
	return formatSnippet(file.goText, tokenFile.Offset(node.Pos()), tokenFile.Offset(node.End()))
}

// findGeneratedOffsets returns the generated offsets of the copied tokens
// that locate the generated code of a construct.
func findGeneratedOffsets(file *generatedFile, found construct) []int {
	isNilOperator := found.keyword == "??" || found.keyword == "?."
	if !isNilOperator {
		return file.sourceMap.GoOffsets(found.start, found.end)
	}
	// The left side of the operator is the last copied text before it.
	lineStart := bytes.LastIndexByte(file.golText[:found.start], '\n') + 1
	offsets := file.sourceMap.GoOffsets(lineStart, found.end)
	return offsets[max(len(offsets)-1, 0):]
}

func findGeneratedNode(path []ast.Node, keyword string) ast.Node {
	switch keyword {
	case "try", "catch", "finally":
		return findTryBlock(path)
	case "t":
		return findSprintfCall(path)
	default:
		return findNilCheck(path)
	}
}

// findTryBlock returns the innermost block with a label that gosling generates
// for a try statement.
func findTryBlock(path []ast.Node) ast.Node {
	for _, node := range path {
		block, ok := node.(*ast.BlockStmt)
		if !ok {
			continue
		}
		for _, statement := range block.List {
			labeled, ok := statement.(*ast.LabeledStmt)
			if ok && hasTryLabel(labeled.Label.Name) {
				return block
			}
		}
	}
	return nil
}

func hasTryLabel(name string) bool {
	return strings.HasPrefix(name, "catch_") || strings.HasPrefix(name, "end_") || strings.HasPrefix(name, "finally_")
}

func findSprintfCall(path []ast.Node) ast.Node {
	for _, node := range path {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			continue
		}
		if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "Sprintf" {
			return call
		}
	}
	return nil
}

// findNilCheck returns the code of ?. or ??. An expression becomes a function
// literal that runs at once. A statement becomes nested if statements.
func findNilCheck(path []ast.Node) ast.Node {
	var outerIf ast.Node
	for pathIndex, node := range path {
		switch typed := node.(type) {
		case *ast.IfStmt:
			if typed.Init != nil {
				outerIf = typed
			}
		case *ast.FuncLit:
			if pathIndex+1 < len(path) {
				if call, ok := path[pathIndex+1].(*ast.CallExpr); ok && call.Fun == typed && len(call.Args) == 0 {
					return call
				}
			}
			return outerIf
		case *ast.FuncDecl:
			return outerIf
		}
	}
	return outerIf
}

// formatSnippet returns goText[start:end] without the indentation of the line
// where it starts. A long snippet ends after maxSnippetLines lines.
func formatSnippet(goText []byte, start, end int) string {
	lineStart := bytes.LastIndexByte(goText[:start], '\n') + 1
	line := string(goText[lineStart:start])
	indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	lines := strings.Split(string(goText[start:end]), "\n")
	for lineIndex := 1; lineIndex < len(lines); lineIndex++ {
		lines[lineIndex] = strings.TrimPrefix(lines[lineIndex], indent)
	}
	if len(lines) > maxSnippetLines {
		lines = append(lines[:maxSnippetLines], "// ...")
	}
	return strings.Join(lines, "\n")
}
