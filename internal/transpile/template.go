package transpile

import (
	"go/ast"
	"go/token"
	"go/types"
	"strconv"
	"strings"
	"unicode"

	"github.com/zacharyhill/gosling/internal/preprocess"
)

// lowerTemplates changes template literal markers into fmt.Sprintf calls.
// It picks each verb from the type of the interpolated expression.
func (lowerer *lowerer) lowerTemplates() {
	var calls []*ast.CallExpr
	ast.Inspect(lowerer.file.ast, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok {
			if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == preprocess.TMarker && !lowerer.dropped(call.Pos()) {
				calls = append(calls, call)
			}
		}
		return true
	})
	if len(calls) == 0 {
		return
	}
	fmtPrefix, ok := lowerer.fmtName(calls[0].Pos())
	if !ok {
		return
	}
	for _, call := range calls {
		lowerer.lowerTemplate(call, fmtPrefix)
	}
}

// lowerTemplate rewrites only the text between interpolated expressions,
// so edits inside the expressions stay.
func (lowerer *lowerer) lowerTemplate(call *ast.CallExpr, fmtPrefix string) {
	pieces := call.Args
	line := lowerer.line(call.Pos())
	if len(pieces) == 1 {
		lowerer.replace(call.Pos(), call.End(), strconv.Quote(rawText(pieces[0])), line)
		return
	}
	var format strings.Builder
	depth := lowerer.depth(call)
	for pieceIndex, piece := range pieces {
		if pieceIndex%2 == 0 {
			format.WriteString(strings.ReplaceAll(rawText(piece), "%", "%%"))
			continue
		}
		pieceType := lowerer.typeInfo.Types[piece].Type
		if bits := floatBits(pieceType); bits > 0 && lowerer.formatFloat(piece, bits, line, depth) {
			format.WriteString("%s")
			continue
		}
		format.WriteString(verb(pieceType))
	}
	lowerer.replace(call.Pos(), pieces[1].Pos(), fmtPrefix+"Sprintf("+strconv.Quote(format.String())+", ", line)
	for pieceIndex := 1; pieceIndex+2 < len(pieces); pieceIndex += 2 {
		lowerer.replace(pieces[pieceIndex].End(), pieces[pieceIndex+2].Pos(), ", ", line)
	}
	lowerer.replace(pieces[len(pieces)-2].End(), call.Rparen+1, ")", line)
}

// formatFloat wraps a float argument in strconv.FormatFloat, because %v
// prints 1e+06 and JavaScript prints 1000000. It reports false if the file
// cannot use strconv.
func (lowerer *lowerer) formatFloat(arg ast.Expr, bits, line, depth int) bool {
	strconvName, ok := lowerer.importNameAt(arg.Pos(), "strconv", "strconv")
	if !ok {
		return false
	}
	openText, closeText := "FormatFloat(", ", 'f', -1, 64)"
	if bits == 32 {
		openText, closeText = "FormatFloat(float64(", "), 'f', -1, 32)"
	}
	lowerer.insertOrdered(arg.Pos(), qualify(strconvName)+openText, line, depth)
	lowerer.insertOrdered(arg.End(), closeText, line, -depth)
	return true
}

// rawText returns a text piece of a template literal, with escapes decoded
// as JavaScript decodes them.
func rawText(arg ast.Expr) string {
	literal, ok := arg.(*ast.BasicLit)
	if !ok || len(literal.Value) < 2 {
		return ""
	}
	return unescape(literal.Value[1 : len(literal.Value)-1])
}

var simpleEscapes = map[byte]string{
	'n': "\n", 't': "\t", 'r': "\r", 'b': "\b", 'f': "\f", 'v': "\v", '0': "\x00",
	'\n': "", // a backslash before a newline continues the line
}

func unescape(raw string) string {
	var decoded strings.Builder
	for byteIndex := 0; byteIndex < len(raw); byteIndex++ {
		if raw[byteIndex] != '\\' || byteIndex+1 == len(raw) {
			decoded.WriteByte(raw[byteIndex])
			continue
		}
		byteIndex++
		if text, ok := simpleEscapes[raw[byteIndex]]; ok {
			decoded.WriteString(text)
			continue
		}
		if decodedRune, consumed, ok := hexEscape(raw[byteIndex:]); ok {
			decoded.WriteRune(decodedRune)
			byteIndex += consumed - 1
			continue
		}
		decoded.WriteByte(raw[byteIndex]) // \\, \$, \", and unknown escapes give the character itself
	}
	return decoded.String()
}

// hexEscape decodes xHH, uHHHH, or u{H...} at the start of escapeText. It
// returns the rune and the number of bytes used.
func hexEscape(escapeText string) (rune, int, bool) {
	var digits string
	var consumed int
	switch {
	case strings.HasPrefix(escapeText, "u{"):
		end := strings.IndexByte(escapeText, '}')
		if end < 0 {
			return 0, 0, false
		}
		digits, consumed = escapeText[2:end], end+1
	case escapeText[0] == 'u' && len(escapeText) >= 5:
		digits, consumed = escapeText[1:5], 5
	case escapeText[0] == 'x' && len(escapeText) >= 3:
		digits, consumed = escapeText[1:3], 3
	default:
		return 0, 0, false
	}
	codePoint, err := strconv.ParseUint(digits, 16, 32)
	if err != nil || codePoint > unicode.MaxRune {
		return 0, 0, false
	}
	return rune(codePoint), consumed, true
}

// floatBits returns 32 or 64 for a plain float type, and 0 otherwise.
func floatBits(goType types.Type) int {
	basic, ok := goType.(*types.Basic)
	switch {
	case !ok || basic.Info()&types.IsFloat == 0:
		return 0
	case basic.Kind() == types.Float32:
		return 32
	}
	return 64
}

// verb picks a readable verb for plain basic types, and %v for all other
// types. So types with String or Error methods format through those methods.
func verb(goType types.Type) string {
	if goType == nil || types.NewMethodSet(goType).Len() > 0 {
		return "%v"
	}
	basic, ok := goType.Underlying().(*types.Basic)
	if !ok {
		return "%v"
	}
	switch {
	case basic.Info()&types.IsString != 0:
		return "%s"
	case basic.Info()&types.IsInteger != 0:
		return "%d"
	case basic.Info()&types.IsBoolean != 0:
		return "%t"
	}
	return "%v"
}

// fmtName returns the qualifier for fmt calls ("fmt." or an alias). It adds
// the import if the file does not have one.
func (lowerer *lowerer) fmtName(position token.Pos) (string, bool) {
	name, ok := lowerer.importNameAt(position, "fmt", "fmt")
	if !ok {
		lowerer.errorf(lowerer.file, position, "template literal needs package fmt, but the name fmt is already used in this file")
		return "", false
	}
	return qualify(name), true
}

func qualify(name string) string {
	if name == "" {
		return "" // dot import
	}
	return name + "."
}
