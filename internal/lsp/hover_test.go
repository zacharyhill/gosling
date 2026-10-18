package lsp

import (
	"strings"
	"testing"
)

const hoverSource = `package p

import (
	"io"
	"strconv"
)

type Profile struct{ Name string }

type User struct{ Profile *Profile }

func parse(text string, c io.Closer) string {
	try {
		const count = strconv.Atoi(text)
		return t` + "`count ${count}`" + `
	} catch (e) {
		return e.Error()
	} finally {
		println("done")
	}
}

func name(user *User) string {
	return user?.Profile?.Name ?? "anon"
}

func closeIt(c io.Closer) {
	c?.Close()
}

func plain() string {
	return t` + "`no values`" + `
}
`

// hoverAt returns the hover text at the first occurrence of needle, moved
// by shift bytes.
func hoverAt(test *testing.T, server *server, file *generatedFile, needle string, shift int) string {
	test.Helper()
	offset := strings.Index(hoverSource, needle)
	if offset < 0 {
		test.Fatalf("%q not found", needle)
	}
	result := server.buildGeneratedHover(file, positionAt([]byte(hoverSource), offset+shift))
	if result == nil {
		return ""
	}
	hover, ok := convertJSON[struct {
		Contents struct {
			Kind  string `json:"kind"`
			Value string `json:"value"`
		} `json:"contents"`
	}](result)
	if !ok || hover.Contents.Kind != "markdown" {
		test.Fatalf("hover = %#v, want markdown contents", result)
	}
	return hover.Contents.Value
}

func TestGeneratedHover(test *testing.T) {
	server, file := newServerFor(test, hoverSource)
	for _, testCase := range []struct {
		name, needle string
		shift        int
		want         []string
	}{
		{"try", "try {", 1, []string{"`try`", "count, err := strconv.Atoi(text)", "goto catch_1", "finally_1:"}},
		{"catch", "catch (e)", 0, []string{"`catch`", "catch_1:\n"}},
		{"finally", "finally {", 3, []string{"`finally`", "finally_1:\n"}},
		{"catch variable", "(e)", 1, []string{"var e error"}},
		{"template tag", "t`count", 0, []string{"template literal", `fmt.Sprintf("count %d", count)`}},
		{"template text", "t`count", 3, []string{`fmt.Sprintf("count %d", count)`}},
		{"template without values", "t`no", 0, []string{"template literal"}},
		{"coalesce", "?? \"anon\"", 1, []string{"`??`", "```go\nfunc() string {", "\treturn \"anon\"\n}()"}},
		{"optional chain", "?.Profile", 0, []string{"`?.`", "if v := user; v != nil {"}},
		{"optional call", "?.Close", 1, []string{"`?.`", "if v := c; v != nil {\n\tv.Close()\n}"}},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			got := hoverAt(test, server, file, testCase.needle, testCase.shift)
			for _, want := range testCase.want {
				if !strings.Contains(got, want) {
					test.Errorf("hover does not contain %q:\n%s", want, got)
				}
			}
		})
	}
}

func TestGeneratedHoverIgnoresOtherText(test *testing.T) {
	server, file := newServerFor(test, hoverSource)
	for _, needle := range []string{"\n\nfunc name", "text string", "Profile struct"} {
		if got := hoverAt(test, server, file, needle, 0); got != "" {
			test.Errorf("hover at %q = %q, want none", needle, got)
		}
	}
}

func TestGeneratedHoverAfterEdit(test *testing.T) {
	server, file := newServerFor(test, hoverSource)
	edited := strings.Replace(hoverSource, "func parse(", "// A comment.\nfunc parse(", 1)
	server.documents[file.golPath].text = []byte(edited)
	offset := strings.Index(edited, "try {")
	result := server.buildGeneratedHover(file, positionAt([]byte(edited), offset))
	hover, ok := convertJSON[struct {
		Contents struct {
			Value string `json:"value"`
		} `json:"contents"`
	}](result)
	if !ok || !strings.Contains(hover.Contents.Value, "goto catch_1") {
		test.Errorf("hover after an edit above = %#v, want the try code", result)
	}
}
