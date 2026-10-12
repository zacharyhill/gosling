package preprocess

import "strings"

// template rewrites t`text ${expr} text` into a TMarker call. Text pieces
// stay raw strings with their escapes, so newlines do not change. The
// transpiler decodes the escapes. The rewrite does not move expression text,
// so its offsets map back to the source.
func (processor *processor) template(tag, literal lexeme) error {
	content := literal.text[1 : len(literal.text)-1]
	base := literal.offset + 1
	var text strings.Builder
	prefix := TMarker + "("
	replaceFrom := tag.offset
	for charIndex := 0; charIndex < len(content); {
		switch {
		case content[charIndex] == '\\' && charIndex+1 < len(content):
			text.WriteString(content[charIndex : charIndex+2]) // an escape, possibly \${
			charIndex += 2
		case strings.HasPrefix(content[charIndex:], "${"):
			exprStart := charIndex + len("${")
			exprEnd, ok := exprExtent(content, exprStart)
			if !ok {
				return &Error{base + charIndex, "unterminated ${ in template literal"}
			}
			if strings.TrimSpace(content[exprStart:exprEnd]) == "" {
				return &Error{base + charIndex, "empty ${} in template literal"}
			}
			processor.replace(replaceFrom, base+exprStart, prefix+"`"+text.String()+"`, ")
			if err := processor.rewriteInterpolation(base+exprStart, base+exprEnd); err != nil {
				return err
			}
			text.Reset()
			prefix = ", "
			replaceFrom = base + exprEnd
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
