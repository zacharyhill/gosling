package preprocess

import (
	"bytes"
	"fmt"
	"go/token"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// VariantsPrefix starts the name of the marker function that lists the
// variants of a union, so the type checker resolves them. Example:
// func __goslingVariants_Shape(Circle, *Square) {}.
const VariantsPrefix = "__goslingVariants_"

// Union records a union type declaration. Src[MarkerStart:MarkerEnd] holds
// the marker function, the separator before it, and the newlines of the
// original declaration.
type Union struct {
	Name                   string
	MarkerStart, MarkerEnd int
}

// union rewrites `type Shape = Circle | *Square` at token tokenIndex into a
// sealed interface and a marker method on each variant. A declaration with
// no top-level | is an ordinary alias, and union does not change it.
func (processor *processor) union(tokenIndex int) (int, error) {
	keyword, name := processor.tokens[tokenIndex], processor.tokenAt(tokenIndex+1)
	assignIndex, end, isUnion := processor.unionHeader(tokenIndex)
	if !isUnion {
		return tokenIndex, nil
	}
	if len(processor.braceStack) > 0 || processor.parens > 0 {
		return 0, &Error{keyword.offset, "union types must be declared at package level with their own type keyword"}
	}
	signature := unionSignature{name: name.text}
	if assignIndex != tokenIndex+2 {
		signature.typeParams = string(processor.src[processor.tokenAt(tokenIndex+2).offset:processor.tokenAt(assignIndex-1).end()])
		signature.paramNames = processor.parseTypeParamNames(tokenIndex+3, assignIndex-1)
	}
	variants, err := processor.variants(assignIndex+1, end, signature.paramNames)
	if err != nil {
		return 0, err
	}
	processor.emitUnion(keyword, processor.tokenAt(end-1), signature, variants)
	return end - 1, nil
}

// unionSignature holds the name of a union and, for a generic union, its type
// parameter list such as "[K comparable, V any]" and the names K and V.
type unionSignature struct {
	name       string
	typeParams string
	paramNames []string
}

// parseTypeParamNames returns the names in the type parameter list in tokens
// [from, to). Each comma-separated group starts with a name.
func (processor *processor) parseTypeParamNames(from, to int) []string {
	var names []string
	depth, isGroupStart := 0, true
	for tokenIndex := from; tokenIndex < to; tokenIndex++ {
		current := processor.tokens[tokenIndex]
		switch current.kind {
		case token.LPAREN, token.LBRACK, token.LBRACE:
			depth++
		case token.RPAREN, token.RBRACK, token.RBRACE:
			depth--
		case token.COMMA:
			isGroupStart = isGroupStart || depth == 0
			continue
		}
		if isGroupStart && current.is(token.IDENT) {
			names = append(names, current.text)
		}
		isGroupStart = false
	}
	return names
}

// unionHeader recognizes `type Name = A | B` or `type Name[T] = A | B` at
// tokenIndex. It returns the index of "=" and the end of the type expression.
func (processor *processor) unionHeader(tokenIndex int) (int, int, bool) {
	if !processor.tokenAt(tokenIndex + 1).is(token.IDENT) {
		return 0, 0, false
	}
	assignIndex := tokenIndex + 2
	if processor.tokenAt(assignIndex).is(token.LBRACK) {
		closeIndex, ok := processor.matchBracket(assignIndex)
		if !ok {
			return 0, 0, false
		}
		assignIndex = closeIndex + 1
	}
	if !processor.tokenAt(assignIndex).is(token.ASSIGN) {
		return 0, 0, false
	}
	end, isUnion := processor.unionExtent(assignIndex + 1)
	return assignIndex, end, isUnion
}

// emitUnion replaces the declaration from keyword to last with the
// interface, one marker method for each variant, and the variants marker
// function.
func (processor *processor) emitUnion(keyword, last lexeme, signature unionSignature, variants []string) {
	declarations := unionDeclarations(signature, variants)
	// The marker keeps the newlines of a multi-line union, so later lines
	// keep their numbers.
	newlines := strings.Repeat("\n", bytes.Count(processor.src[keyword.offset:last.end()], []byte("\n")))
	marker := fmt.Sprintf("; func %s%s%s(%s) {}%s", VariantsPrefix, signature.name, signature.typeParams, strings.Join(variants, ", "), newlines)
	declarationText := strings.Join(declarations, "; ")
	start := processor.replace(keyword.offset, last.end(), declarationText+marker)

	offset := start
	for _, declaration := range declarations[:len(declarations)-1] {
		offset += len(declaration)
		processor.result.Splits = append(processor.result.Splits, offset)
		offset += len("; ")
	}
	processor.result.Unions = append(processor.result.Unions, Union{
		Name:        signature.name,
		MarkerStart: start + len(declarationText),
		MarkerEnd:   start + len(declarationText) + len(marker),
	})
}

// unionDeclarations returns the interface and the marker methods. In a
// generic union, the marker method takes the type parameters. So Ok[string]
// does not satisfy Result[int].
func unionDeclarations(signature unionSignature, variants []string) []string {
	method := "is" + upperFirst(signature.name)
	paramList := strings.Join(signature.paramNames, ", ")
	declarations := []string{fmt.Sprintf("type %s%s interface{ %s(%s) }", signature.name, signature.typeParams, method, paramList)}
	// A pointer variant gets a pointer receiver, so the value type does not
	// also satisfy the union.
	for _, variant := range variants {
		declarations = append(declarations, fmt.Sprintf("func (%s) %s(%s) {}", variant, method, paramList))
	}
	return declarations
}

// unionExtent finds the end of the type expression that starts at token
// tokenIndex. It also reports whether the expression has a top-level |.
func (processor *processor) unionExtent(tokenIndex int) (int, bool) {
	depth, isUnion := 0, false
	for ; tokenIndex < len(processor.tokens); tokenIndex++ {
		switch processor.tokens[tokenIndex].kind {
		case token.LPAREN, token.LBRACK, token.LBRACE:
			depth++
		case token.RPAREN, token.RBRACK, token.RBRACE:
			if depth == 0 {
				return tokenIndex, isUnion
			}
			depth--
		case token.SEMICOLON:
			if depth == 0 {
				return tokenIndex, isUnion
			}
		case token.OR:
			isUnion = isUnion || depth == 0
		}
	}
	return tokenIndex, isUnion
}

// variants parses `A | *B | pkg.C` in tokens [from, to). In a generic
// union, each variant has the type parameters as its type arguments.
func (processor *processor) variants(from, to int, paramNames []string) ([]string, error) {
	var variants []string
	for tokenIndex := from; tokenIndex < to; {
		start := processor.tokenAt(tokenIndex)
		if processor.tokenAt(tokenIndex).is(token.MUL) {
			tokenIndex++
		}
		if !processor.tokenAt(tokenIndex).is(token.IDENT) {
			return nil, &Error{start.offset, "union variants must be type names"}
		}
		tokenIndex++
		if processor.tokenAt(tokenIndex).is(token.PERIOD) && processor.tokenAt(tokenIndex+1).is(token.IDENT) {
			tokenIndex += 2
		}
		if len(paramNames) > 0 {
			afterTypeArgsIndex, err := processor.checkVariantTypeArgs(start, tokenIndex, to, paramNames)
			if err != nil {
				return nil, err
			}
			tokenIndex = afterTypeArgsIndex
		}
		variants = append(variants, string(processor.src[start.offset:processor.tokenAt(tokenIndex-1).end()]))
		if tokenIndex < to {
			if !processor.tokenAt(tokenIndex).is(token.OR) {
				return nil, &Error{processor.tokenAt(tokenIndex).offset, "union variants must be type names"}
			}
			tokenIndex++
		}
	}
	return variants, nil
}

// checkVariantTypeArgs checks that the variant that starts at start has the
// type arguments [paramNames...] at token tokenIndex. It returns the index
// after the closing bracket.
func (processor *processor) checkVariantTypeArgs(start lexeme, tokenIndex, to int, paramNames []string) (int, error) {
	end := processor.tokenAt(tokenIndex - 1).end()
	closeIndex, ok := processor.matchBracket(tokenIndex)
	if ok && closeIndex < to {
		if slices.Equal(processor.parseTypeArgNames(tokenIndex+1, closeIndex), paramNames) {
			return closeIndex + 1, nil
		}
		end = processor.tokenAt(closeIndex).end()
	}
	return 0, &Error{start.offset, fmt.Sprintf("union variant %s must have the type arguments [%s]", processor.src[start.offset:end], strings.Join(paramNames, ", "))}
}

// parseTypeArgNames returns the names in a list of type arguments in tokens
// [from, to). It returns nil if an argument is not a single name.
func (processor *processor) parseTypeArgNames(from, to int) []string {
	var names []string
	for tokenIndex := from; tokenIndex < to; tokenIndex += 2 {
		separator := processor.tokenAt(tokenIndex + 1)
		if !processor.tokens[tokenIndex].is(token.IDENT) || (tokenIndex+1 < to && !separator.is(token.COMMA)) {
			return nil
		}
		names = append(names, processor.tokens[tokenIndex].text)
	}
	return names
}

func (processor *processor) matchBracket(openIndex int) (int, bool) {
	if !processor.tokenAt(openIndex).is(token.LBRACK) {
		return 0, false
	}
	depth := 0
	for tokenIndex := openIndex; tokenIndex < len(processor.tokens); tokenIndex++ {
		switch processor.tokens[tokenIndex].kind {
		case token.LBRACK:
			depth++
		case token.RBRACK:
			depth--
			if depth == 0 {
				return tokenIndex, true
			}
		}
	}
	return 0, false
}

func upperFirst(word string) string {
	firstRune, runeWidth := utf8.DecodeRuneInString(word)
	return string(unicode.ToUpper(firstRune)) + word[runeWidth:]
}
