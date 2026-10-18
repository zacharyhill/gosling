package transpile

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/zacharyhill/gosling/internal/preprocess"
)

var (
	markerRE               = regexp.MustCompile(`\b__gosling\w*`)
	spreadMismatchRE       = regexp.MustCompile(`^in call to ` + preprocess.CatMarker + `, type (.+) of (.+) does not match inferred type (.+) for S$`)
	structSpreadMismatchRE = regexp.MustCompile(`^in call to ` + preprocess.WithMarker + `, type (.+) of .+ does not match inferred type (.+) for V$`)
	callbackMismatchRE     = regexp.MustCompile(`^in call to ` + arrayMarkerPrefix + `(\w+), type (func\(.+?) of .+ does not match inferred type (func\(.+) for func\(.+$`)
)

// cleanMessage changes marker names in a type checker message back to the
// user's syntax.
func cleanMessage(message string) string {
	if strings.Contains(message, arrayMarkerPrefix+"forEach") && strings.Contains(message, "used as value") {
		return "forEach can only be used as a statement"
	}
	if match := spreadMismatchRE.FindStringSubmatch(message); match != nil {
		return formatSpreadMismatch(match[1], match[2], match[3])
	}
	if match := structSpreadMismatchRE.FindStringSubmatch(message); match != nil {
		return fmt.Sprintf("struct spread of type %s into %s", match[2], match[1])
	}
	if match := callbackMismatchRE.FindStringSubmatch(message); match != nil {
		return formatCallbackMismatch(arrayMethodName(match[1]), match[2], match[3])
	}
	return cleanMarkers(strings.ReplaceAll(message, "."+preprocess.OMarker+".", "?."))
}

func formatCallbackMismatch(method, callbackType, want string) string {
	if !hasResult(callbackType) && hasResult(want) {
		return fmt.Sprintf("%s callback must return a value", method)
	}
	return fmt.Sprintf("%s callback has type %s, want %s", method, callbackType, want)
}

// hasResult reports whether a function type, written as func(...) or
// func(...) R, has a result.
func hasResult(funcType string) bool {
	depth := 0
	for index, char := range funcType {
		switch char {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return index < len(funcType)-1
			}
		}
	}
	return false
}

func formatSpreadMismatch(partType, part, want string) string {
	elements, isElementPart := strings.CutPrefix(part, preprocess.OneMarker+"(")
	if isElementPart && strings.HasPrefix(partType, "[]") && strings.HasPrefix(want, "[]") {
		return fmt.Sprintf("slice spread element %s has type %s, want %s", cleanMarkers(strings.TrimSuffix(elements, ")")), strings.TrimPrefix(partType, "[]"), strings.TrimPrefix(want, "[]"))
	}
	return fmt.Sprintf("slice spread part ...%s has type %s, want %s", cleanMarkers(part), partType, want)
}

// cleanMarkers rewrites each marker in message. A marker call becomes the
// syntax that produced it. Another marker becomes the name of its feature.
func cleanMarkers(message string) string {
	var cleaned strings.Builder
	for {
		location := markerRE.FindStringIndex(message)
		if location == nil {
			cleaned.WriteString(message)
			return cleaned.String()
		}
		cleaned.WriteString(message[:location[0]])
		name := message[location[0]:location[1]]
		rest := skipTypeArgs(message[location[1]:])
		if text, after, ok := cleanCall(name, rest); ok {
			if (name == coalesceMarker || name == coalescePtrMarker) && shouldParenthesize(cleaned.String(), after) {
				text = "(" + text + ")"
			}
			cleaned.WriteString(text)
			message = after
			continue
		}
		cleaned.WriteString(markerDisplayName(name))
		message = rest
	}
}

const operatorChars = "+-*/%&|^<>=!"

// shouldParenthesize reports whether a ?? expression between before and after
// needs parentheses. Each operator next to it binds tighter than ??.
func shouldParenthesize(before, after string) bool {
	before, after = strings.TrimRight(before, " "), strings.TrimLeft(after, " ")
	return (before != "" && strings.ContainsAny(before[len(before)-1:], operatorChars)) ||
		(after != "" && strings.ContainsAny(after[:1], operatorChars+".["))
}

func skipTypeArgs(text string) string {
	if !strings.HasPrefix(text, "[") {
		return text
	}
	depth := 0
	for index, char := range text {
		switch char {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return text[index+1:]
			}
		}
	}
	return text
}

// cleanCall rewrites a marker call whose arguments start text. It reports
// false if the marker has no call form or the call is incomplete.
func cleanCall(name, text string) (string, string, bool) {
	rawArgs, after, ok := splitCallArgs(text)
	if !ok {
		return "", "", false
	}
	args := make([]string, len(rawArgs))
	for argIndex, rawArg := range rawArgs {
		args[argIndex] = cleanMarkers(rawArg)
	}
	switch {
	case name == preprocess.TMarker:
		return buildTemplateText(args), after, true
	case name == preprocess.OneMarker:
		return strings.Join(args, ", "), after, true
	case name == preprocess.CatMarker:
		for argIndex, rawArg := range rawArgs {
			if !strings.HasPrefix(rawArg, preprocess.OneMarker) {
				args[argIndex] = "..." + args[argIndex]
			}
		}
		return "[" + strings.Join(args, ", ") + "]", after, true
	case name == hoistMarker && len(args) == 1:
		return args[0], after, true
	case (name == coalesceMarker || name == coalescePtrMarker) && len(args) == 2:
		return args[0] + " ?? " + args[1], after, true
	case strings.HasPrefix(name, arrayMarkerPrefix) && len(args) > 0:
		return args[0] + "." + markerDisplayName(name) + "(" + strings.Join(args[1:], ", ") + ")", after, true
	}
	return "", "", false
}

// splitCallArgs splits the arguments of the call that starts text. It skips
// brackets and quoted text inside the arguments.
func splitCallArgs(text string) ([]string, string, bool) {
	if !strings.HasPrefix(text, "(") {
		return nil, "", false
	}
	var args []string
	depth, argStart := 0, 1
	for index := 0; index < len(text); index++ {
		switch char := text[index]; char {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
			if depth == 0 {
				args = appendArg(args, text[argStart:index])
				return args, text[index+1:], true
			}
		case ',':
			if depth == 1 {
				args = appendArg(args, text[argStart:index])
				argStart = index + 1
			}
		case '"', '\'', '`':
			closeIndex := quoteEnd(text, index)
			if closeIndex < 0 {
				return nil, "", false
			}
			index = closeIndex
		}
	}
	return nil, "", false
}

func appendArg(args []string, arg string) []string {
	if arg = strings.TrimSpace(arg); arg == "" {
		return args
	}
	return append(args, arg)
}

// quoteEnd returns the index of the quote that closes the quoted text at
// start, or -1.
func quoteEnd(text string, start int) int {
	quote := text[start]
	for index := start + 1; index < len(text); index++ {
		switch {
		case text[index] == '\\' && quote != '`':
			index++
		case text[index] == quote:
			return index
		}
	}
	return -1
}

// buildTemplateText rebuilds a template literal from its marker arguments. Text
// and expressions alternate.
func buildTemplateText(args []string) string {
	var template strings.Builder
	template.WriteString("t`")
	for argIndex, arg := range args {
		if argIndex%2 == 0 {
			template.WriteString(strings.TrimSuffix(strings.TrimPrefix(arg, "`"), "`"))
			continue
		}
		template.WriteString("${" + arg + "}")
	}
	template.WriteString("`")
	return template.String()
}

// markerDisplayName names the feature that a marker stands for. Markers
// with no such name are removed.
func markerDisplayName(name string) string {
	switch name {
	case preprocess.MapMarker:
		return "map"
	case preprocess.UntypedMarker, preprocess.ResultMarker:
		return "any"
	case preprocess.TMarker:
		return "t"
	case preprocess.CatMarker, preprocess.OneMarker:
		return "slice spread"
	case preprocess.WithMarker:
		return "struct spread"
	case coalesceMarker, coalescePtrMarker:
		return "??"
	case preprocess.OMarker:
		return "?."
	}
	if method, ok := strings.CutPrefix(name, arrayMarkerPrefix); ok {
		return arrayMethodName(method)
	}
	if union, ok := strings.CutPrefix(name, preprocess.VariantsPrefix); ok {
		return union
	}
	return ""
}

// arrayMethodName removes the variant suffix from an array marker name.
func arrayMethodName(suffixed string) string {
	for _, suffix := range []string{"", "PI", "P", "I"} {
		if method, ok := strings.CutSuffix(suffixed, suffix); ok {
			if _, isMethod := arrayMethods[method]; isMethod {
				return method
			}
		}
	}
	return suffixed
}
