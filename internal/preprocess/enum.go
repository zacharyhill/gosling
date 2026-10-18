package preprocess

import (
	"bytes"
	"fmt"
	"go/token"
	"strings"
)

// Enum records an enum declaration. A numeric enum has a String method
// stub at Src[MethodStart:MethodEnd]. A string enum has no method.
type Enum struct {
	Name     string
	Members  []string
	IsString bool
	// MethodStart and MethodEnd are offsets into Src.
	MethodStart, MethodEnd int
}

// MemberName returns the Go constant for member of the enum named enumName.
func MemberName(enumName, member string) string {
	return enumName + upperFirst(member)
}

// enumMember is one member of an enum, as token indexes. value is 0 if
// the member has no value.
type enumMember struct {
	name, value, end int
}

// isEnum reports whether the token at tokenIndex starts `enum Name {`.
func (processor *processor) isEnum(tokenIndex int) bool {
	return processor.tokens[tokenIndex].isIdent("enum") && processor.tokenAt(tokenIndex+1).is(token.IDENT) &&
		processor.tokenAt(tokenIndex+2).is(token.LBRACE) && processor.atStmtStart(tokenIndex)
}

// rewriteEnum rewrites the enum declaration at token tokenIndex:
//
//	enum Color { Red, Green }
//
// becomes
//
//	type Color int; const ( ColorRed Color = iota; ColorGreen ); func (Color) String() string { return "" }
//
// A string enum gets type string and no String method. It returns the
// index of the closing brace.
func (processor *processor) rewriteEnum(tokenIndex int) (int, error) {
	keyword, name := processor.tokens[tokenIndex], processor.tokens[tokenIndex+1]
	if len(processor.braceStack) > 0 || processor.parens > 0 {
		return 0, &Error{keyword.offset, "enums must be declared at package level"}
	}
	members, closeIndex, err := processor.parseEnumMembers(tokenIndex + 2)
	if err != nil {
		return 0, err
	}
	if len(members) == 0 {
		return 0, &Error{keyword.offset, fmt.Sprintf("enum %s has no members", name.text)}
	}
	isString, err := processor.checkEnumValues(members)
	if err != nil {
		return 0, err
	}
	underlying := "int"
	if isString {
		underlying = "string"
	}
	processor.replace(keyword.offset, keyword.end(), "type")
	start := processor.replace(name.end(), processor.tokens[tokenIndex+2].end(), " "+underlying+"; const (")
	processor.result.Splits = append(processor.result.Splits, start+len(" "+underlying))
	enum := Enum{Name: name.text, IsString: isString}
	for memberIndex, member := range members {
		enum.Members = append(enum.Members, processor.tokens[member.name].text)
		processor.replaceMember(name.text, members, memberIndex)
		processor.replaceSeparator(member.end)
	}
	processor.closeEnum(&enum, processor.tokens[closeIndex])
	processor.result.Enums = append(processor.result.Enums, enum)
	return closeIndex, nil
}

// closeEnum ends the const block at closeBrace. A numeric enum also gets a
// String method stub, which lowering replaces.
func (processor *processor) closeEnum(enum *Enum, closeBrace lexeme) {
	if enum.IsString {
		processor.replace(closeBrace.offset, closeBrace.end(), ")")
		return
	}
	method := fmt.Sprintf("func (%s) String() string { return \"\" }", enum.Name)
	methodStart := processor.replace(closeBrace.offset, closeBrace.end(), "); "+method) + len("); ")
	processor.result.Splits = append(processor.result.Splits, methodStart-len("; "))
	enum.MethodStart, enum.MethodEnd = methodStart, methodStart+len(method)
}

// parseEnumMembers parses the members in the braces that open at token
// openIndex. It returns the members and the index of the closing brace.
func (processor *processor) parseEnumMembers(openIndex int) ([]enumMember, int, error) {
	var members []enumMember
	tokenIndex := openIndex + 1
	for {
		for processor.tokenAt(tokenIndex).is(token.SEMICOLON) {
			tokenIndex++
		}
		current := processor.tokenAt(tokenIndex)
		if current.is(token.RBRACE) {
			return members, tokenIndex, nil
		}
		if !current.is(token.IDENT) {
			return nil, 0, &Error{current.offset, "expected an enum member name"}
		}
		member := enumMember{name: tokenIndex}
		if processor.tokenAt(tokenIndex + 1).is(token.ASSIGN) {
			member.value = tokenIndex + 2
		}
		end, err := processor.findEnumMemberEnd(tokenIndex + 1)
		if err != nil {
			return nil, 0, err
		}
		member.end = end
		members = append(members, member)
		tokenIndex = end
		if processor.tokenAt(end).is(token.COMMA) {
			tokenIndex++
		}
	}
}

// findEnumMemberEnd returns the index of the comma, semicolon, or closing
// brace that ends the member value starting at token tokenIndex.
func (processor *processor) findEnumMemberEnd(tokenIndex int) (int, error) {
	depth := 0
	for ; tokenIndex < len(processor.tokens); tokenIndex++ {
		switch processor.tokens[tokenIndex].kind {
		case token.LPAREN, token.LBRACK, token.LBRACE:
			depth++
		case token.RPAREN, token.RBRACK:
			depth--
		case token.RBRACE:
			if depth == 0 {
				return tokenIndex, nil
			}
			depth--
		case token.COMMA, token.SEMICOLON:
			if depth == 0 {
				return tokenIndex, nil
			}
		}
	}
	return 0, &Error{processor.tokenAt(tokenIndex).offset, "unterminated enum"}
}

// checkEnumValues reports whether the enum is a string enum. The first
// member decides. It reports a member whose value does not match.
func (processor *processor) checkEnumValues(members []enumMember) (bool, error) {
	isString := processor.hasStringValue(members[0])
	for _, member := range members[1:] {
		name := processor.tokens[member.name]
		switch hasString := processor.hasStringValue(member); {
		case isString && !hasString:
			return false, &Error{name.offset, fmt.Sprintf("member %s of a string enum needs a string value", name.text)}
		case !isString && hasString:
			return false, &Error{name.offset, fmt.Sprintf("member %s of a numeric enum cannot have a string value", name.text)}
		}
	}
	return isString, nil
}

func (processor *processor) hasStringValue(member enumMember) bool {
	return member.value > 0 && member.value+1 == member.end && processor.tokens[member.value].is(token.STRING)
}

// replaceMember writes the constant name, type, and implicit value of a
// member. A numeric enum without values uses iota. Otherwise a member
// without a value is one more than the member before it, as in TypeScript.
func (processor *processor) replaceMember(enumName string, members []enumMember, memberIndex int) {
	member := members[memberIndex]
	name := processor.tokens[member.name]
	hasValues := false
	for _, other := range members {
		hasValues = hasValues || other.value > 0
	}
	suffix := " " + enumName
	switch {
	case member.value > 0:
	case !hasValues && memberIndex == 0:
		suffix += " = iota"
	case !hasValues:
		suffix = ""
	case memberIndex == 0:
		suffix += " = 0"
	default:
		previous := processor.tokens[members[memberIndex-1].name].text
		suffix += " = " + MemberName(enumName, previous) + " + 1"
	}
	// An added prefix keeps the member name in the output, so the source map
	// covers it.
	if constName := MemberName(enumName, name.text); strings.HasSuffix(constName, name.text) {
		processor.replace(name.offset, name.offset, enumName)
	} else {
		processor.replace(name.offset, name.end(), constName)
	}
	if suffix != "" {
		processor.replace(name.end(), name.end(), suffix)
	}
}

// replaceSeparator changes the comma at token tokenIndex into "; " if the
// next member is on the same line. It removes other commas.
func (processor *processor) replaceSeparator(tokenIndex int) {
	comma, next := processor.tokens[tokenIndex], processor.tokenAt(tokenIndex+1)
	if !comma.is(token.COMMA) {
		return
	}
	isLineEnd := bytes.IndexByte(processor.src[comma.end():next.offset], '\n') >= 0
	if isLineEnd || next.is(token.RBRACE) || next.is(token.SEMICOLON) {
		processor.replace(comma.offset, comma.end(), "")
		return
	}
	processor.result.Splits = append(processor.result.Splits, processor.replace(comma.offset, next.offset, "; "))
}
