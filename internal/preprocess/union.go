package preprocess

import (
	"bytes"
	"fmt"
	"go/token"
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
	switch {
	case assignIndex != tokenIndex+2:
		return 0, &Error{keyword.offset, "generic union types are not supported"}
	case len(processor.braceStack) > 0 || processor.parens > 0:
		return 0, &Error{keyword.offset, "union types must be declared at package level with their own type keyword"}
	}
	variants, err := processor.variants(assignIndex+1, end)
	if err != nil {
		return 0, err
	}
	processor.emitUnion(keyword, processor.tokenAt(end-1), name.text, variants)
	return end - 1, nil
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
func (processor *processor) emitUnion(keyword, last lexeme, name string, variants []string) {
	declarations := unionDeclarations(name, variants)
	// The marker keeps the newlines of a multi-line union, so later lines
	// keep their numbers.
	newlines := strings.Repeat("\n", bytes.Count(processor.src[keyword.offset:last.end()], []byte("\n")))
	marker := fmt.Sprintf("; func %s%s(%s) {}%s", VariantsPrefix, name, strings.Join(variants, ", "), newlines)
	declarationText := strings.Join(declarations, "; ")
	start := processor.replace(keyword.offset, last.end(), declarationText+marker)

	offset := start
	for _, declaration := range declarations[:len(declarations)-1] {
		offset += len(declaration)
		processor.result.Splits = append(processor.result.Splits, offset)
		offset += len("; ")
	}
	processor.result.Unions = append(processor.result.Unions, Union{
		Name:        name,
		MarkerStart: start + len(declarationText),
		MarkerEnd:   start + len(declarationText) + len(marker),
	})
}

func unionDeclarations(name string, variants []string) []string {
	method := "is" + upperFirst(name)
	declarations := []string{fmt.Sprintf("type %s interface{ %s() }", name, method)}
	// A pointer variant gets a pointer receiver, so the value type does not
	// also satisfy the union.
	for _, variant := range variants {
		declarations = append(declarations, fmt.Sprintf("func (%s) %s() {}", variant, method))
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

// variants parses `A | *B | pkg.C` in tokens [from, to).
func (processor *processor) variants(from, to int) ([]string, error) {
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

func (processor *processor) matchBracket(openIndex int) (int, bool) {
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
