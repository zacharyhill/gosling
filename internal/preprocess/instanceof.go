package preprocess

import (
	"go/token"
	"go/types"
	"unicode"
)

// Narrowing records an if statement whose condition is `x instanceof T`.
// Var is the offset of the narrowed x, or -1 after !. With an else branch,
// Src[CopyStart:CopyEnd] copies a temporary to x, so the else branch sees
// the original x.
type Narrowing struct {
	If, Var            int
	CopyStart, CopyEnd int
}

const errInstanceofPlace = "instanceof is only supported as the condition of an if statement"

// instanceofCondition holds the token indexes of `x instanceof T`. The type
// is in tokens [typeStart, typeEnd).
type instanceofCondition struct {
	subject, typeStart, typeEnd int
	isNegated                   bool
}

// isInstanceof reports whether the token at tokenIndex is the instanceof
// operator. As an operator, it follows an operand.
func (processor *processor) isInstanceof(tokenIndex int) bool {
	if tokenIndex == 0 || !processor.tokens[tokenIndex].isIdent("instanceof") || processor.isMethodName(tokenIndex) {
		return false
	}
	switch processor.tokens[tokenIndex-1].kind {
	case token.IDENT, token.RPAREN, token.RBRACK:
		return true
	}
	return false
}

// isMethodName reports whether the token at tokenIndex follows the receiver
// in a method declaration, as in func (r R) instanceof().
func (processor *processor) isMethodName(tokenIndex int) bool {
	if !processor.tokens[tokenIndex-1].is(token.RPAREN) {
		return false
	}
	depth := 0
	for receiverIndex := tokenIndex - 1; receiverIndex >= 0; receiverIndex-- {
		depth -= getDepthChange(processor.tokens[receiverIndex].kind)
		if depth <= 0 {
			return depth == 0 && receiverIndex > 0 && processor.tokens[receiverIndex-1].is(token.FUNC)
		}
	}
	return false
}

// rewriteInstanceof rewrites the if statement at token ifIndex if its condition is
// `x instanceof T`, `(x instanceof T)`, or `!(x instanceof T)`. It returns
// the index of the last token before the body.
func (processor *processor) rewriteInstanceof(ifIndex int) (int, error) {
	braceIndex := processor.findConditionEnd(ifIndex + 1)
	operatorIndex := -1
	for tokenIndex := ifIndex + 1; tokenIndex < braceIndex && operatorIndex < 0; tokenIndex++ {
		if processor.isInstanceof(tokenIndex) {
			operatorIndex = tokenIndex
		}
	}
	if operatorIndex < 0 {
		return ifIndex, nil
	}
	if braceIndex == len(processor.tokens) {
		return 0, &Error{processor.tokens[operatorIndex].offset, "the if statement has no block after the instanceof condition"}
	}
	condition, err := processor.parseInstanceof(ifIndex+1, braceIndex, operatorIndex)
	if err != nil {
		return 0, err
	}
	processor.emitNarrowing(ifIndex, braceIndex, condition)
	return braceIndex - 1, nil
}

// findConditionEnd returns the index of the "{" that starts the body of an if
// statement whose condition starts at token from. The "{" of an interface or
// struct type is part of the condition.
func (processor *processor) findConditionEnd(from int) int {
	depth := 0
	for tokenIndex := from; tokenIndex < len(processor.tokens); tokenIndex++ {
		current := processor.tokens[tokenIndex]
		previousKind := processor.tokens[tokenIndex-1].kind
		isTypeBody := previousKind == token.INTERFACE || previousKind == token.STRUCT
		if current.is(token.LBRACE) && depth == 0 && !isTypeBody {
			return tokenIndex
		}
		depth += getDepthChange(current.kind)
	}
	return len(processor.tokens)
}

// getDepthChange returns how much kind changes the bracket depth.
func getDepthChange(kind token.Token) int {
	switch kind {
	case token.LPAREN, token.LBRACK, token.LBRACE:
		return 1
	case token.RPAREN, token.RBRACK, token.RBRACE:
		return -1
	}
	return 0
}

func (processor *processor) parseInstanceof(from, to, operatorIndex int) (instanceofCondition, error) {
	operator := processor.tokens[operatorIndex]
	from, to = processor.stripParens(from, to)
	isNegated := processor.tokens[from].is(token.NOT)
	if isNegated {
		innerFrom, innerTo := processor.stripParens(from+1, to)
		if innerFrom == from+1 {
			return instanceofCondition{}, &Error{processor.tokens[from].offset, "write !(x instanceof T)"}
		}
		from, to = innerFrom, innerTo
	}
	switch {
	case operatorIndex < from || operatorIndex >= to || !processor.isPrimary(from, operatorIndex) || !processor.isPrimary(operatorIndex+1, to):
		return instanceofCondition{}, &Error{operator.offset, errInstanceofPlace}
	case operatorIndex != from+1 || !processor.tokens[from].is(token.IDENT):
		return instanceofCondition{}, &Error{processor.tokens[from].offset, "the left side of instanceof must be a variable name"}
	}
	typeStart, typeEnd := processor.stripParens(operatorIndex+1, to)
	if typeStart == typeEnd {
		return instanceofCondition{}, &Error{operator.offset, "instanceof needs a type"}
	}
	return instanceofCondition{subject: from, typeStart: typeStart, typeEnd: typeEnd, isNegated: isNegated}, nil
}

// stripParens removes the parentheses around tokens [from, to).
func (processor *processor) stripParens(from, to int) (int, int) {
	for from < to && processor.tokens[from].is(token.LPAREN) {
		closeIndex, _ := processor.matchParen(from)
		if closeIndex != to-1 {
			break
		}
		from, to = from+1, to-1
	}
	return from, to
}

// isPrimary reports whether tokens [from, to) have no operator outside
// brackets, other than "." and "*". So they are an operand or a type, not a
// larger expression.
func (processor *processor) isPrimary(from, to int) bool {
	depth := 0
	for tokenIndex := from; tokenIndex < to; tokenIndex++ {
		kind := processor.tokens[tokenIndex].kind
		if change := getDepthChange(kind); change != 0 {
			depth += change
			continue
		}
		if depth == 0 && kind.IsOperator() && kind != token.PERIOD && kind != token.MUL {
			return false
		}
	}
	return true
}

// emitNarrowing rewrites the condition into `x, ok := x.(T); ok`. The
// transpiler later uses errors.AsType for errors and drops unused names.
func (processor *processor) emitNarrowing(ifIndex, braceIndex int, condition instanceofCondition) {
	conditionStart := processor.tokens[ifIndex+1].offset
	subject := processor.tokens[condition.subject]
	okName := processor.getNarrowingName("ok")
	hasElse := processor.hasElse(braceIndex)
	declaredName := subject.text
	switch {
	case condition.isNegated:
		declaredName = "_"
	case hasElse:
		declaredName = processor.getNarrowingName(processor.buildTemporaryName(condition))
	}
	start := processor.replace(conditionStart, subject.offset, declaredName+", "+okName+" := ")
	narrowing := Narrowing{If: start - (conditionStart - processor.tokens[ifIndex].offset), Var: start}
	processor.replace(subject.end(), processor.tokens[condition.typeStart].offset, ".(")
	testText := okName
	if condition.isNegated {
		testText = "!" + okName
		narrowing.Var = -1
	}
	processor.replace(processor.tokens[condition.typeEnd-1].end(), processor.tokens[braceIndex].offset, "); "+testText+" ")
	if hasElse && !condition.isNegated {
		brace := processor.tokens[braceIndex]
		copyText := " " + subject.text + " := " + declaredName + ";"
		narrowing.CopyStart = processor.replace(brace.end(), brace.end(), copyText)
		narrowing.CopyEnd = narrowing.CopyStart + len(copyText)
		narrowing.Var = narrowing.CopyStart + len(" ")
	}
	processor.result.Narrowings = append(processor.result.Narrowings, narrowing)
}

// getNarrowingName returns a name that the source does not use, the same one
// for each base. Each if header declares its own variable, so all can share
// the name.
func (processor *processor) getNarrowingName(base string) string {
	if name, ok := processor.narrowingNames[base]; ok {
		return name
	}
	name := processor.fresh(base)
	processor.narrowingNames[base] = name
	return name
}

// hasElse reports whether an else follows the block that opens at token
// braceIndex.
func (processor *processor) hasElse(braceIndex int) bool {
	depth := 0
	for tokenIndex := braceIndex; tokenIndex < len(processor.tokens); tokenIndex++ {
		switch processor.tokens[tokenIndex].kind {
		case token.LBRACE:
			depth++
		case token.RBRACE:
			depth--
			if depth == 0 {
				return processor.tokenAt(tokenIndex + 1).is(token.ELSE)
			}
		}
	}
	return false
}

// buildTemporaryName names the temporary after the type: circle for Circle,
// pathError for *fs.PathError.
func (processor *processor) buildTemporaryName(condition instanceofCondition) string {
	name, depth := "", 0
	for tokenIndex := condition.typeStart; tokenIndex < condition.typeEnd; tokenIndex++ {
		current := processor.tokens[tokenIndex]
		depth += getDepthChange(current.kind)
		if current.is(token.IDENT) && depth == 0 {
			name = lowerFirst(current.text)
		}
	}
	if name == "" || token.IsKeyword(name) || types.Universe.Lookup(name) != nil {
		return "narrowed"
	}
	return name
}

// lowerFirst turns the leading upper case letters of word to lower case.
// It keeps the last one if a lower case letter follows, so URLError
// becomes urlError.
func lowerFirst(word string) string {
	runes := []rune(word)
	upperEnd := 0
	for upperEnd < len(runes) && unicode.IsUpper(runes[upperEnd]) {
		upperEnd++
	}
	if upperEnd > 1 && upperEnd < len(runes) {
		upperEnd--
	}
	for runeIndex := range upperEnd {
		runes[runeIndex] = unicode.ToLower(runes[runeIndex])
	}
	return string(runes)
}
