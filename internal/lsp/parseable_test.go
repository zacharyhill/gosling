package lsp

import (
	"testing"

	"github.com/zacharyhill/gosling/internal/transpile"
)

const parsedVersion = "package p\n\nfunc a() int {\n\treturn 1\n}\n\nfunc b() int {\n\treturn 2\n}\n"

func TestParseableVersion(test *testing.T) {
	for _, testCase := range []struct {
		name, edited, want string
	}{
		{
			name:   "error in a changed line",
			edited: "package p\n\nfunc added() int {\n\treturn 3\n}\n\nfunc a() int {\n\treturn 1 +\n}\n\nfunc b() int {\n\treturn added()\n}\n",
			want:   "package p\n\nfunc added() int {\n\treturn 3\n}\n\nfunc a() int {\n\treturn 1\n}\n\nfunc b() int {\n\treturn added()\n}\n",
		},
		{
			name:   "error at the end of the file",
			edited: "package p\n\nfunc added() int {\n\treturn 3\n}\n\nfunc a() int {\n\treturn 1\n}\n\nfunc b() int {\n\tif true {\n\treturn 2\n}\n",
			want:   "package p\n\nfunc added() int {\n\treturn 3\n}\n\nfunc a() int {\n\treturn 1\n}\n\nfunc b() int {\n\treturn 2\n}\n",
		},
		{
			name:   "gosling error",
			edited: "package p\n\nfunc a() int {\n\tx := 1\n\treturn x\n}\n\nfunc b() int {\n\treturn 4\n}\n",
			want:   "package p\n\nfunc a() int {\n\treturn 1\n}\n\nfunc b() int {\n\treturn 4\n}\n",
		},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			input := transpile.Input{Path: "p.gol", Src: []byte(testCase.edited)}
			got := buildParseableVersion(input, []byte(parsedVersion), transpile.FindParseErrors(input))
			if string(got) != testCase.want {
				test.Errorf("got\n%s\nwant\n%s", got, testCase.want)
			}
		})
	}
}

func TestLineHunks(test *testing.T) {
	oldText := []byte("a\nb\nc\nd\n")
	newText := []byte("a\nB\nc\nd\ne\n")
	hunks := findLineHunks(oldText, newText)
	want := []hunk{
		{oldStart: 2, oldEnd: 4, newStart: 2, newEnd: 4},
		{oldStart: 8, oldEnd: 8, newStart: 8, newEnd: 10},
	}
	if len(hunks) != len(want) || hunks[0] != want[0] || hunks[1] != want[1] {
		test.Errorf("hunks = %+v, want %+v", hunks, want)
	}
	if got := applyHunks(oldText, newText, hunks[1:]); string(got) != "a\nb\nc\nd\ne\n" {
		test.Errorf("applyHunks = %q", got)
	}
}
