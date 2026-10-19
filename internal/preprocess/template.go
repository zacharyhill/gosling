package preprocess

import (
	"bytes"
	"fmt"
	"go/token"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// interpolation holds the source offsets of the value in a ${value} or a
// ${value:spec}, and the spec.
type interpolation struct {
	start, end int
	spec       string
	hasSpec    bool
	specOffset int
}

// splitTemplate splits a template literal into its raw text pieces and the
// interpolations between them. It returns one more piece than
// interpolations.
func (processor *processor) splitTemplate(literal lexeme) ([]string, []interpolation, error) {
	content := literal.text[1 : len(literal.text)-1]
	contentStart := literal.offset + 1
	var pieces []string
	var interpolations []interpolation
	var text strings.Builder
	for charIndex := 0; charIndex < len(content); {
		switch {
		case content[charIndex] == '\\' && charIndex+1 < len(content) && processor.src[contentStart+charIndex+1] == '`':
			text.WriteString(`\x60`) // a raw string cannot hold the backtick itself
			charIndex += 2
		case content[charIndex] == '\\' && charIndex+1 < len(content):
			text.WriteString(content[charIndex : charIndex+2]) // an escape, possibly \${
			charIndex += 2
		case strings.HasPrefix(content[charIndex:], "${"):
			exprStart := charIndex + len("${")
			exprEnd, ok := exprExtent(content, exprStart)
			if !ok {
				return nil, nil, &Error{contentStart + charIndex, "unterminated ${ in template literal"}
			}
			valueEnd, spec := findFormatSpec(content, exprStart, exprEnd)
			if strings.TrimSpace(content[exprStart:valueEnd]) == "" {
				return nil, nil, &Error{contentStart + charIndex, "empty ${} in template literal"}
			}
			pieces = append(pieces, text.String())
			interpolations = append(interpolations, interpolation{
				start: contentStart + exprStart, end: contentStart + valueEnd,
				spec: spec, hasSpec: valueEnd < exprEnd, specOffset: contentStart + valueEnd,
			})
			text.Reset()
			charIndex = exprEnd + len("}")
		default:
			text.WriteByte(content[charIndex])
			charIndex++
		}
	}
	return append(pieces, text.String()), interpolations, nil
}

// template rewrites t`text ${expr} text` into a TMarker call. Text pieces
// stay raw strings with their escapes, so newlines do not change. The
// transpiler decodes the escapes. The rewrite does not move expression text,
// so its offsets map back to the source.
func (processor *processor) template(tag, literal lexeme) error {
	pieces, interpolations, err := processor.splitTemplate(literal)
	if err != nil {
		return err
	}
	prefix, replaceFrom := TMarker+"(", tag.offset
	for pieceIndex, interpolated := range interpolations {
		valuePrefix, valueSuffix, err := buildFormatWrapper(interpolated.spec, interpolated.hasSpec, interpolated.specOffset)
		if err != nil {
			return err
		}
		processor.replace(replaceFrom, interpolated.start, prefix+"`"+pieces[pieceIndex]+"`, "+valuePrefix)
		if err := processor.rewriteInterpolation(interpolated.start, interpolated.end); err != nil {
			return err
		}
		prefix, replaceFrom = valueSuffix+", ", interpolated.end
	}
	processor.replace(replaceFrom, literal.end(), prefix+"`"+pieces[len(pieces)-1]+"`)")
	return nil
}

// rewriteTaggedTemplate rewrites tag`a ${x} b` into
// tag([]string{"a ", " b"}, x). It decodes the escapes, so the strings have
// no raw newlines. The newlines of each piece go between the arguments, so
// lines do not change.
func (processor *processor) rewriteTaggedTemplate(literal lexeme) error {
	pieces, interpolations, err := processor.splitTemplate(literal)
	if err != nil {
		return err
	}
	quoted := make([]string, len(pieces))
	for pieceIndex, piece := range pieces {
		quoted[pieceIndex] = strconv.Quote(Unescape(piece))
	}
	prefix, replaceFrom := "([]string{"+strings.Join(quoted, ", ")+"}", literal.offset
	for pieceIndex, interpolated := range interpolations {
		if interpolated.hasSpec {
			return &Error{interpolated.specOffset, "a tagged template cannot have a format; use t`...`"}
		}
		// One newline stays, so a line directive before the value follows
		// a newline. gofmt moves a directive that follows a comma.
		paddingLines, keptNewline := extractNewlines(pieces[pieceIndex]), ""
		if paddingLines != "" {
			paddingLines, keptNewline = paddingLines[1:], "\n"
		}
		processor.replacePadded(replaceFrom, interpolated.start, prefix+", "+keptNewline, paddingLines, "")
		if err := processor.rewriteInterpolation(interpolated.start, interpolated.end); err != nil {
			return err
		}
		prefix, replaceFrom = "", interpolated.end
	}
	lastPaddingLines := extractNewlines(pieces[len(pieces)-1])
	if lastPaddingLines != "" {
		lastPaddingLines = "," + lastPaddingLines // the comma stops a semicolon at the newline
	}
	processor.replacePadded(replaceFrom, literal.end(), prefix, lastPaddingLines, ")")
	return nil
}

// replacePadded replaces src[start:end] with before+padding+after. The output
// drops the padding newlines.
func (processor *processor) replacePadded(start, end int, before, padding, after string) {
	newStart := processor.replace(start, end, before+padding+after)
	processor.recordNewlines(newStart+len(before), padding)
}

// extractNewlines returns only the newlines of text.
func extractNewlines(text string) string {
	return strings.Repeat("\n", strings.Count(text, "\n"))
}

// isGoRawString reports whether the raw string after the name at token
// nameIndex is plain Go: a struct field tag or an import path.
func (processor *processor) isGoRawString(nameIndex int) bool {
	if nameIndex > 0 && processor.tokens[nameIndex-1].is(token.IMPORT) {
		return true
	}
	depth := 0
	for openIndex := nameIndex - 1; openIndex > 0; openIndex-- {
		switch processor.tokens[openIndex].kind {
		case token.RPAREN, token.RBRACK, token.RBRACE:
			depth++
		case token.LPAREN, token.LBRACK, token.LBRACE:
			if depth > 0 {
				depth--
				continue
			}
			tokenBeforeOpen := processor.tokens[openIndex-1]
			return tokenBeforeOpen.is(token.STRUCT) || tokenBeforeOpen.is(token.IMPORT)
		}
	}
	return false
}

// formatSpecRE matches a printf verb without the %, with optional flags,
// width, and precision.
var formatSpecRE = regexp.MustCompile(`^[-+# 0]*[0-9]*(\.[0-9]*)?[bcdeEfFgGoOpqstTUvxX]$`)

// buildFormatWrapper returns the text before and after a value with a format
// spec. It returns empty text if the value has no spec.
func buildFormatWrapper(spec string, hasSpec bool, specOffset int) (string, string, error) {
	if !hasSpec {
		return "", "", nil
	}
	if !formatSpecRE.MatchString(spec) {
		return "", "", &Error{specOffset, fmt.Sprintf("invalid format %s in template literal; use a printf verb such as .2f or 05d", spec)}
	}
	return FormatMarker + "(", ", " + strconv.Quote(spec) + ")", nil
}

// findFormatSpec finds a format spec, as in ${price:.2f}, in the
// interpolation content[from:to]. Go expressions have a colon only inside
// brackets, and each ternary ? takes one colon. So the spec starts at the
// first other colon outside brackets, quotes, and regex literals. It
// returns the end of the value and the spec without spaces.
func findFormatSpec(content string, from, to int) (int, string) {
	masked := content[:from] + string(MaskRegexes([]byte(content[from:to])))
	depth, openTernaries := 0, 0
	for charIndex := from; charIndex < to; charIndex++ {
		switch masked[charIndex] {
		case '{', '(', '[':
			depth++
		case '}', ')', ']':
			depth--
		case '"', '\'':
			if end, ok := quoteEnd(masked, charIndex); ok {
				charIndex = end
			}
		case '?':
			if charIndex+1 < to && (masked[charIndex+1] == '?' || masked[charIndex+1] == '.') {
				charIndex++
			} else if depth == 0 {
				openTernaries++
			}
		case ':':
			switch {
			case depth > 0:
			case openTernaries > 0:
				openTernaries--
			default:
				return charIndex, strings.TrimSpace(content[charIndex+1 : to])
			}
		}
	}
	return to, ""
}

// exprExtent finds the "}" that closes the interpolation starting at from.
// It skips nested brackets and quoted strings.
func exprExtent(content string, from int) (int, bool) {
	depth := 0
	for charIndex := from; charIndex < len(content); charIndex++ {
		switch char := content[charIndex]; char {
		case '{', '(', '[':
			depth++
		case ')', ']':
			depth--
		case '}':
			if depth == 0 {
				return charIndex, true
			}
			depth--
		case '"', '\'':
			end, ok := quoteEnd(content, charIndex)
			if !ok {
				return 0, false
			}
			charIndex = end
		}
	}
	return 0, false
}

// quoteEnd returns the index of the quote closing the string or rune
// literal that opens at openIndex.
func quoteEnd(content string, openIndex int) (int, bool) {
	quote := content[openIndex]
	for charIndex := openIndex + 1; charIndex < len(content); charIndex++ {
		switch content[charIndex] {
		case '\\':
			charIndex++
		case quote:
			return charIndex, true
		}
	}
	return 0, false
}

// MaskBackticks returns src with each escaped backtick in a template literal
// changed to a space. The Go scanner then reads each template literal as one
// raw string. MaskBackticks returns src itself if it has no such backtick.
func MaskBackticks(src []byte) []byte {
	var masked []byte
	for offset := 0; offset < len(src); offset++ {
		switch {
		case bytes.HasPrefix(src[offset:], []byte("//")):
			offset = findEnd(src, offset, "\n")
		case bytes.HasPrefix(src[offset:], []byte("/*")):
			offset = findEnd(src, offset+len("/*"), "*/")
		case src[offset] == '"' || src[offset] == '\'':
			offset = findLiteralEnd(src, offset)
		case src[offset] == '`' && !isTemplateStart(src, offset):
			offset = findEnd(src, offset+1, "`")
		case src[offset] == '`':
			offset, masked = maskTemplateBackticks(src, masked, offset)
		}
	}
	if masked == nil {
		return src
	}
	return masked
}

// maskTemplateBackticks masks the escaped backticks of the template literal
// that opens at openOffset. It returns the offset of the closing backtick and
// the masked copy. It makes the copy at the first escaped backtick.
func maskTemplateBackticks(src, masked []byte, openOffset int) (int, []byte) {
	offset := openOffset + 1
	for ; offset < len(src) && src[offset] != '`'; offset++ {
		if src[offset] != '\\' || offset+1 == len(src) {
			continue
		}
		offset++
		if src[offset] != '`' {
			continue
		}
		if masked == nil {
			masked = bytes.Clone(src)
		}
		masked[offset] = ' '
	}
	return offset, masked
}

// findEnd returns the offset of the last byte of the first terminator at or
// after from, or the last offset of src.
func findEnd(src []byte, from int, terminator string) int {
	end := bytes.Index(src[from:], []byte(terminator))
	if end < 0 {
		return len(src) - 1
	}
	return from + end + len(terminator) - 1
}

// findLiteralEnd returns the offset of the quote that closes the string or
// rune literal at openOffset. An unterminated literal ends at its line.
func findLiteralEnd(src []byte, openOffset int) int {
	for offset := openOffset + 1; offset < len(src); offset++ {
		switch src[offset] {
		case '\\':
			offset++
		case src[openOffset], '\n':
			return offset
		}
	}
	return len(src) - 1
}

// isTemplateStart reports whether the backtick at offset follows the tag t.
func isTemplateStart(src []byte, offset int) bool {
	if offset == 0 || src[offset-1] != 't' {
		return false
	}
	if offset == 1 {
		return true
	}
	byteBeforeTag := src[offset-2]
	return byteBeforeTag != '_' && byteBeforeTag < utf8.RuneSelf && !unicode.IsLetter(rune(byteBeforeTag)) && !unicode.IsDigit(rune(byteBeforeTag))
}

var simpleEscapes = map[byte]string{
	'n': "\n", 't': "\t", 'r': "\r", 'b': "\b", 'f': "\f", 'v': "\v", '0': "\x00",
	'\n': "", // a backslash before a newline continues the line
}

// Unescape decodes the escapes in a raw text piece of a template literal, as
// JavaScript decodes them.
func Unescape(raw string) string {
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
