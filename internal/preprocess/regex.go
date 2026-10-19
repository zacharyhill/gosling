package preprocess

import (
	"errors"
	"fmt"
	"go/token"
	"regexp"
	"strconv"
	"strings"
)

// RegexMarker replaces a regex literal. /a+/i becomes
// __goslingRegexp(`(?i)a+`).
const RegexMarker = "__goslingRegexp"

// supportedRegexFlags are the JavaScript flags that Go regexp also has.
const supportedRegexFlags = "ims"

// UsesRegex reports whether src has a regex literal.
func UsesRegex(src []byte) bool {
	for _, current := range tokenize(MaskBackticks(src)) {
		if isRegex(current) {
			return true
		}
	}
	return false
}

func isRegex(current lexeme) bool {
	return current.is(token.ILLEGAL) && strings.HasPrefix(current.text, "/")
}

// tokenize returns the tokens of src. A regex literal is one ILLEGAL token
// whose text starts with "/".
func tokenize(src []byte) []lexeme {
	lexemes, regexEnds, _ := scanMasked(src)
	for lexemeIndex, current := range lexemes {
		if end, ok := regexEnds[current.offset]; ok {
			lexemes[lexemeIndex] = lexeme{offset: current.offset, kind: token.ILLEGAL, text: string(src[current.offset:end])}
		}
	}
	return lexemes
}

// MaskRegexes returns a copy of src in which each regex literal is a string
// literal of the same length, so a Go scanner reads src correctly.
func MaskRegexes(src []byte) []byte {
	_, _, masked := scanMasked(src)
	return masked
}

// scanMasked finds the regex literals in src. It returns the tokens of the
// masked source, the end of each literal by its start, and the masked source.
func scanMasked(src []byte) ([]lexeme, map[int]int, []byte) {
	lexemes := scanTokens(src)
	regexEnds := map[int]int{}
	masked := src
	for {
		start, found := findRegexStart(lexemes, regexEnds)
		if !found {
			return lexemes, regexEnds, masked
		}
		end := scanRegex(src, start)
		if len(regexEnds) == 0 {
			masked = append([]byte(nil), src...)
		}
		regexEnds[start] = end
		maskRegex(masked, start, end)
		// The literal can contain text that looks like a comment, so scan
		// again with the literal masked.
		lexemes = scanTokens(masked)
	}
}

// maskRegex replaces src[start:end] with a string literal, or with a number
// if the range is too short. The scanner then sees one operand.
func maskRegex(src []byte, start, end int) {
	for offset := start; offset < end; offset++ {
		src[offset] = '_'
	}
	if end-start < len(`""`) {
		src[start] = '0'
		return
	}
	src[start], src[end-1] = '"', '"'
}

// findRegexStart returns the offset of the first "/" that starts an operand
// and is not a known regex literal. As in JavaScript, a "/" after an operand
// is a division.
func findRegexStart(lexemes []lexeme, regexEnds map[int]int) (int, bool) {
	for lexemeIndex, current := range lexemes {
		if !current.is(token.QUO) && !current.is(token.QUO_ASSIGN) {
			continue
		}
		if _, known := regexEnds[current.offset]; known {
			continue
		}
		if lexemeIndex == 0 || !isOperandEnd(lexemes[lexemeIndex-1]) {
			return current.offset, true
		}
	}
	return 0, false
}

// scanRegex returns the end of the regex literal at src[start]. For an
// unterminated literal, it returns the end of the line.
func scanRegex(src []byte, start int) int {
	bodyEnd, closed := scanRegexBody(src, start)
	if !closed {
		return bodyEnd
	}
	end := bodyEnd + 1
	for end < len(src) && isIdentByte(src[end]) {
		end++
	}
	return end
}

// scanRegexBody returns the offset of the "/" that closes the literal at
// src[start]. A "/" in a character class or after a backslash does not
// close it. It reports false if the line ends first.
func scanRegexBody(src []byte, start int) (int, bool) {
	inClass := false
	for offset := start + 1; offset < len(src); offset++ {
		switch src[offset] {
		case '\n':
			return offset, false
		case '\\':
			if offset+1 < len(src) && src[offset+1] != '\n' {
				offset++
			}
		case '[':
			inClass = true
		case ']':
			inClass = false
		case '/':
			if !inClass {
				return offset, true
			}
		}
	}
	return len(src), false
}

func isIdentByte(char byte) bool {
	return char == '_' || char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9'
}

// rewriteRegex rewrites a regex literal into a marker call.
func (processor *processor) rewriteRegex(literal lexeme) error {
	pattern, err := parseRegex(literal.text)
	if err != nil {
		return &Error{literal.offset, err.Error()}
	}
	if _, err := regexp.Compile(pattern); err != nil {
		return &Error{literal.offset, err.Error()}
	}
	call := RegexMarker + "(" + quotePattern(pattern) + ")"
	if literal.offset > 0 && isIdentByte(processor.src[literal.offset-1]) {
		call = " " + call
	}
	processor.replace(literal.offset, literal.end(), call)
	return nil
}

// parseRegex converts the text of a regex literal into a Go pattern. The
// flags become a (?flags) prefix.
func parseRegex(text string) (string, error) {
	bodyEnd, closed := scanRegexBody([]byte(text), 0)
	if !closed {
		return "", errors.New("unterminated regex literal")
	}
	flags := text[bodyEnd+1:]
	for flagIndex, flag := range flags {
		switch {
		case !strings.ContainsRune(supportedRegexFlags, flag):
			return "", fmt.Errorf("regex flag %c is not supported; use i, m, or s", flag)
		case strings.ContainsRune(flags[:flagIndex], flag):
			return "", fmt.Errorf("regex flag %c is repeated", flag)
		}
	}
	pattern := unescapeSlashes(text[1:bodyEnd])
	if flags != "" {
		pattern = "(?" + flags + ")" + pattern
	}
	return pattern, nil
}

// unescapeSlashes changes each \/ in body into /. Go regexp accepts \/ too,
// but a plain / reads better.
func unescapeSlashes(body string) string {
	var pattern strings.Builder
	for offset := 0; offset < len(body); offset++ {
		if body[offset] == '\\' && offset+1 < len(body) {
			offset++
			if body[offset] != '/' {
				pattern.WriteByte('\\')
			}
		}
		pattern.WriteByte(body[offset])
	}
	return pattern.String()
}

func quotePattern(pattern string) string {
	if strings.Contains(pattern, "`") {
		return strconv.Quote(pattern)
	}
	return "`" + pattern + "`"
}
