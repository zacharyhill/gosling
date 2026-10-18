package preprocess

import (
	"bytes"
	"strings"
	"unicode"
	"unicode/utf8"
)

// template rewrites t`text ${expr} text` into a TMarker call. Text pieces
// stay raw strings with their escapes, so newlines do not change. The
// transpiler decodes the escapes. The rewrite does not move expression text,
// so its offsets map back to the source.
func (processor *processor) template(tag, literal lexeme) error {
	content := literal.text[1 : len(literal.text)-1]
	contentStart := literal.offset + 1
	var text strings.Builder
	prefix := TMarker + "("
	replaceFrom := tag.offset
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
				return &Error{contentStart + charIndex, "unterminated ${ in template literal"}
			}
			if strings.TrimSpace(content[exprStart:exprEnd]) == "" {
				return &Error{contentStart + charIndex, "empty ${} in template literal"}
			}
			processor.replace(replaceFrom, contentStart+exprStart, prefix+"`"+text.String()+"`, ")
			if err := processor.rewriteInterpolation(contentStart+exprStart, contentStart+exprEnd); err != nil {
				return err
			}
			text.Reset()
			prefix = ", "
			replaceFrom = contentStart + exprEnd
			charIndex = exprEnd + len("}")
		default:
			text.WriteByte(content[charIndex])
			charIndex++
		}
	}
	processor.replace(replaceFrom, literal.end(), prefix+"`"+text.String()+"`)")
	return nil
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
