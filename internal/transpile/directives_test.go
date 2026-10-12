package transpile

import (
	"go/format"
	"slices"
	"testing"
)

// TestRestoreDirectiveLines checks that every token keeps the line it had
// before gofmt moved the directives.
func TestRestoreDirectiveLines(test *testing.T) {
	unformatted := []byte("package p\n\n" +
		"func total(sum func(...int) int) int {\n" +
		"\tcount := sum(1, func() int { return 1 }()/*line a.gol:9*/, 2)\n" +
		"\tswitch count {\n" +
		"\tcase 1:\n" +
		"\t\tcount++\n" +
		"\t/*line a.gol:20*/default:\n" +
		"\t}\n" +
		"\tfor range ([]int{1}) {\n" +
		"\t}\n" +
		"\treturn count/*line a.gol:30*/}\n")
	formatted, err := format.Source(unformatted)
	if err != nil {
		test.Fatal(err)
	}
	want := scanSource(unformatted).lines(true)
	before := scanSource(formatted).lines(true)
	if slices.Equal(before, want) {
		test.Fatal("gofmt no longer moves the directives, so this test checks nothing")
	}
	got := scanSource(restoreDirectiveLines(unformatted, formatted)).lines(true)
	if !slices.Equal(got, want) {
		test.Errorf("token lines %v, want %v\n%s", got, want, restoreDirectiveLines(unformatted, formatted))
	}
}
