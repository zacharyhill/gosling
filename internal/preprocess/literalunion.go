package preprocess

import (
	"fmt"
	"go/constant"
	"go/token"
	"strings"
	"unicode"
)

const errMixedLiterals = "literal union members must all be strings or all be integers"

type literal struct {
	text      string
	value     constant.Value
	constName string
}

// isLiteralStart reports whether the union member at token tokenIndex is a
// literal, as in `type Method = "GET" | "POST"`.
func (processor *processor) isLiteralStart(tokenIndex int) bool {
	switch processor.tokenAt(tokenIndex).kind {
	case token.STRING, token.INT, token.FLOAT, token.IMAG, token.CHAR, token.SUB:
		return true
	}
	return false
}

// rewriteLiteralUnion replaces the union declaration at keyword with a named
// string or int type, one constant for each literal, and a Valid method.
func (processor *processor) rewriteLiteralUnion(keyword lexeme, nameIndex, assignIndex, end int) error {
	switch {
	case len(processor.braceStack) > 0 || processor.parens > 0:
		return &Error{keyword.offset, "union types must be declared at package level"}
	case assignIndex != nameIndex+1:
		return &Error{keyword.offset, "a literal union cannot be generic"}
	}
	name, last := processor.tokens[nameIndex].text, processor.tokenAt(end-1)
	literals, err := processor.parseLiterals(name, assignIndex+1, end)
	if err != nil {
		return err
	}
	declarations, constNames := buildLiteralDeclarations(name, literals)
	// The last separator also becomes a line break. The Valid method spans
	// several lines after gofmt, so the next line needs a line directive.
	// The output keeps the line count of a multi-line union.
	newlines := processor.collectNewlines(keyword.offset, last.end())
	start := processor.replace(keyword.offset, last.end(), strings.Join(declarations, "; ")+"; "+newlines)
	offset := start
	for _, declaration := range declarations {
		offset += len(declaration)
		processor.result.Splits = append(processor.result.Splits, offset)
		offset += len("; ")
	}
	processor.result.Unions = append(processor.result.Unions, Union{Name: name, Constants: constNames})
	return nil
}

// buildLiteralDeclarations returns the type, constant, and Valid method
// declarations of a literal union, and the constant names.
func buildLiteralDeclarations(name string, literals []literal) ([]string, []string) {
	baseType := "int"
	if literals[0].value.Kind() == constant.String {
		baseType = "string"
	}
	constSpecs := make([]string, len(literals))
	constNames := make([]string, len(literals))
	for literalIndex, member := range literals {
		constSpecs[literalIndex] = fmt.Sprintf("%s %s = %s", member.constName, name, member.text)
		constNames[literalIndex] = member.constName
	}
	receiver := string(unicode.ToLower([]rune(name)[0]))
	declarations := []string{
		fmt.Sprintf("type %s %s", name, baseType),
		fmt.Sprintf("const (%s)", strings.Join(constSpecs, "; ")),
		fmt.Sprintf("func (%s %s) Valid() bool { switch %s { case %s: return true }; return false }", receiver, name, receiver, strings.Join(constNames, ", ")),
	}
	return declarations, constNames
}

// parseLiterals parses `"a" | "b"` or `1 | -2` in tokens [from, to).
func (processor *processor) parseLiterals(union string, from, to int) ([]literal, error) {
	var literals []literal
	for tokenIndex := from; tokenIndex < to; tokenIndex++ {
		start := processor.tokenAt(tokenIndex)
		isNegative := start.is(token.SUB)
		if isNegative {
			tokenIndex++
		}
		current := processor.tokenAt(tokenIndex)
		isString := current.is(token.STRING) && !isNegative
		isUnsupportedKind := !isString && !current.is(token.INT)
		isMixedKind := len(literals) > 0 && isString != (literals[0].value.Kind() == constant.String)
		if isUnsupportedKind || isMixedKind {
			return nil, &Error{start.offset, errMixedLiterals}
		}
		member, err := newLiteral(union, string(processor.src[start.offset:current.end()]), current, isNegative)
		if err != nil {
			return nil, &Error{start.offset, err.Error()}
		}
		if err := checkLiteralConflict(literals, member, union); err != nil {
			return nil, &Error{start.offset, err.Error()}
		}
		literals = append(literals, member)
		if tokenIndex+1 < to && !processor.tokenAt(tokenIndex+1).is(token.OR) {
			return nil, &Error{processor.tokenAt(tokenIndex + 1).offset, errMixedLiterals}
		}
		tokenIndex++
	}
	return literals, nil
}

// checkLiteralConflict reports a member with the same value or the same
// constant name as an earlier literal.
func checkLiteralConflict(literals []literal, member literal, union string) error {
	for _, previous := range literals {
		switch {
		case constant.Compare(previous.value, token.EQL, member.value):
			return fmt.Errorf("duplicate literal %s in union %s", member.text, union)
		case previous.constName == member.constName:
			return fmt.Errorf("literals %s and %s both give the constant name %s", previous.text, member.text, member.constName)
		}
	}
	return nil
}

func newLiteral(union, text string, current lexeme, isNegative bool) (literal, error) {
	value := constant.MakeFromLiteral(current.text, current.kind, 0)
	if isNegative {
		value = constant.UnaryOp(token.SUB, value, 0)
	}
	suffix := strings.Replace(value.ExactString(), "-", "Neg", 1)
	if value.Kind() == constant.String {
		suffix = formatConstantSuffix(constant.StringVal(value))
	}
	if suffix == "" {
		return literal{}, fmt.Errorf("cannot make a constant name from the literal %s", text)
	}
	return literal{text: text, value: value, constName: union + suffix}, nil
}

// formatConstantSuffix turns "not-found" into "NotFound". Each run of
// letters and digits starts with an upper case letter.
func formatConstantSuffix(text string) string {
	var suffix strings.Builder
	for _, word := range strings.FieldsFunc(text, func(char rune) bool {
		return !unicode.IsLetter(char) && !unicode.IsDigit(char)
	}) {
		suffix.WriteString(upperFirst(word))
	}
	return suffix.String()
}
