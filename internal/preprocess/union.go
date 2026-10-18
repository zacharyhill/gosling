package preprocess

import (
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

// Union records a union type declaration. Src[Start:] holds its interface
// declaration. Src[MarkerStart:MarkerEnd] holds the marker function, the
// separator before it, and the newlines of the original declaration. A
// literal union has no marker function. Constants holds the names of its
// constants.
type Union struct {
	Name                   string
	Start                  int
	MarkerStart, MarkerEnd int
	Constants              []string
}

// union rewrites `type Shape = Circle | *Square` at token tokenIndex into a
// sealed interface and a marker method on each variant. A declaration with
// no top-level | is an ordinary alias, and union does not change it. A
// union of literals becomes a named type with constants.
func (processor *processor) union(tokenIndex int) (int, error) {
	if processor.tokenAt(tokenIndex + 1).is(token.LPAREN) {
		return processor.rewriteUnionGroup(tokenIndex)
	}
	keyword := processor.tokens[tokenIndex]
	assignIndex, end, isUnion := processor.unionHeader(tokenIndex + 1)
	if !isUnion {
		return tokenIndex, nil
	}
	if processor.isLiteralStart(assignIndex + 1) {
		return end - 1, processor.rewriteLiteralUnion(keyword, tokenIndex+1, assignIndex, end)
	}
	signature, variants, err := processor.parseUnion(keyword, tokenIndex+1, assignIndex, end)
	if err != nil {
		return 0, err
	}
	last := processor.tokenAt(end - 1)
	declaration := "type " + formatUnionInterface(signature)
	methods := buildUnionMethods(signature, variants)
	// The marker keeps the newlines of a multi-line union, so later lines
	// keep their numbers.
	methods.text += processor.collectNewlines(keyword.offset, last.end())
	start := processor.replace(keyword.offset, last.end(), declaration+methods.text)
	processor.recordUnion(signature.name, start, start+len(declaration), methods)
	return end - 1, nil
}

// rewriteUnionGroup rewrites the unions in the `type ( ... )` group at token
// tokenIndex. Each union becomes an interface in the group. Its methods
// follow the closing parenthesis, because a group cannot hold functions.
func (processor *processor) rewriteUnionGroup(tokenIndex int) (int, error) {
	closeIndex, _ := processor.matchParen(tokenIndex + 1)
	if closeIndex < 0 {
		return tokenIndex, nil
	}
	unions, err := processor.rewriteGroupedUnions(tokenIndex+2, closeIndex)
	if err != nil {
		return 0, err
	}
	if len(unions) == 0 {
		return tokenIndex, nil
	}
	var methodsText strings.Builder
	for _, union := range unions {
		methodsText.WriteString(union.methods.text)
	}
	methodsStart := processor.replace(processor.tokens[closeIndex].end(), processor.tokens[closeIndex].end(), methodsText.String())
	for _, union := range unions {
		processor.recordUnion(union.name, union.start, methodsStart, union.methods)
		methodsStart += len(union.methods.text)
	}
	return closeIndex, nil
}

// groupedUnion is a union in a type group. start is the output offset of
// its interface.
type groupedUnion struct {
	name    string
	start   int
	methods unionText
}

// rewriteGroupedUnions replaces each union spec in tokens [from, to) with
// its interface. It returns the unions in order.
func (processor *processor) rewriteGroupedUnions(from, to int) ([]groupedUnion, error) {
	var unions []groupedUnion
	for specIndex := from; specIndex < to; {
		assignIndex, end, isUnion := processor.unionHeader(specIndex)
		if !isUnion {
			end, _ = processor.unionExtent(specIndex)
			specIndex = end + 1
			continue
		}
		nameToken := processor.tokens[specIndex]
		if processor.isLiteralStart(assignIndex + 1) {
			return nil, &Error{nameToken.offset, "a literal union needs its own type keyword, not a type group"}
		}
		signature, variants, err := processor.parseUnion(nameToken, specIndex, assignIndex, end)
		if err != nil {
			return nil, err
		}
		// The interface keeps the newlines of a multi-line union.
		lastEnd := processor.tokenAt(end - 1).end()
		interfaceText := formatUnionInterface(signature) + processor.collectNewlines(nameToken.offset, lastEnd)
		start := processor.replace(nameToken.offset, lastEnd, interfaceText)
		unions = append(unions, groupedUnion{signature.name, start, buildUnionMethods(signature, variants)})
		specIndex = end + 1
	}
	return unions, nil
}

// parseUnion checks the union whose name is at token nameIndex and returns
// its signature and variants. Errors are at start, the start of the
// declaration.
func (processor *processor) parseUnion(start lexeme, nameIndex, assignIndex, end int) (unionSignature, []string, error) {
	if len(processor.braceStack) > 0 || processor.parens > 0 {
		return unionSignature{}, nil, &Error{start.offset, "union types must be declared at package level"}
	}
	signature := unionSignature{name: processor.tokens[nameIndex].text}
	if assignIndex != nameIndex+1 {
		signature.typeParams = string(processor.src[processor.tokenAt(nameIndex+1).offset:processor.tokenAt(assignIndex-1).end()])
		signature.paramNames = processor.parseTypeParamNames(nameIndex+2, assignIndex-1)
	}
	variants, err := processor.variants(assignIndex+1, end, signature.paramNames)
	return signature, variants, err
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

// unionHeader recognizes `Name = A | B` or `Name[T] = A | B` at nameIndex.
// It returns the index of "=" and the end of the type expression.
func (processor *processor) unionHeader(nameIndex int) (int, int, bool) {
	if !processor.tokenAt(nameIndex).is(token.IDENT) {
		return 0, 0, false
	}
	assignIndex := nameIndex + 1
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

// unionText is the text that follows the interface of a union: one marker
// method for each variant, then the variants marker function. Each starts
// with "; ". splits holds the offsets of the separators before the methods.
type unionText struct {
	text        string
	splits      []int
	markerStart int
}

// formatUnionInterface returns the interface type spec of a union. In a
// generic union, the marker method takes the type parameters. So Ok[string]
// does not satisfy Result[int].
func formatUnionInterface(signature unionSignature) string {
	return fmt.Sprintf("%s%s interface{ is%s(%s) }", signature.name, signature.typeParams, upperFirst(signature.name), strings.Join(signature.paramNames, ", "))
}

// buildUnionMethods returns the marker methods and the variants marker
// function of a union.
func buildUnionMethods(signature unionSignature, variants []string) unionText {
	var methodsText strings.Builder
	var splits []int
	method, paramList := "is"+upperFirst(signature.name), strings.Join(signature.paramNames, ", ")
	// A pointer variant gets a pointer receiver, so the value type does not
	// also satisfy the union.
	for _, variant := range variants {
		splits = append(splits, methodsText.Len())
		fmt.Fprintf(&methodsText, "; func (%s) %s(%s) {}", variant, method, paramList)
	}
	markerStart := methodsText.Len()
	fmt.Fprintf(&methodsText, "; func %s%s%s(%s) {}", VariantsPrefix, signature.name, signature.typeParams, strings.Join(variants, ", "))
	return unionText{text: methodsText.String(), splits: splits, markerStart: markerStart}
}

// recordUnion records the union whose interface starts at output offset
// start and whose methods start at methodsStart.
func (processor *processor) recordUnion(name string, start, methodsStart int, methods unionText) {
	for _, split := range methods.splits {
		processor.result.Splits = append(processor.result.Splits, methodsStart+split)
	}
	processor.result.Unions = append(processor.result.Unions, Union{
		Name:        name,
		Start:       start,
		MarkerStart: methodsStart + methods.markerStart,
		MarkerEnd:   methodsStart + len(methods.text),
	})
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
