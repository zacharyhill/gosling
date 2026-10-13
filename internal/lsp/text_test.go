package lsp

import (
	"strings"
	"testing"
)

func TestPositionConversion(test *testing.T) {
	text := []byte("ab\néx\U0001F600y\nlast")
	testCases := []struct {
		offset   int
		position position
	}{
		{0, position{0, 0}},
		{2, position{0, 2}},
		{3, position{1, 0}},
		{len("ab\né"), position{1, 1}},
		{len("ab\néx"), position{1, 2}},
		{len("ab\néx\U0001F600"), position{1, 4}},
		{len(text), position{2, 4}},
	}
	for _, testCase := range testCases {
		if got := positionAt(text, testCase.offset); got != testCase.position {
			test.Errorf("positionAt(%d) = %v, want %v", testCase.offset, got, testCase.position)
		}
		got, ok := offsetAt(text, testCase.position)
		if !ok || got != testCase.offset {
			test.Errorf("offsetAt(%v) = %d, %v, want %d", testCase.position, got, ok, testCase.offset)
		}
	}
	if _, ok := offsetAt(text, position{5, 0}); ok {
		test.Error("offsetAt past the last line must fail")
	}
	if got, ok := offsetAt(text, position{0, 99}); !ok || got != 2 {
		test.Errorf("offsetAt past the end of a line = %d, %v, want the line end", got, ok)
	}
}

func TestTextShift(test *testing.T) {
	oldText := []byte("func a() {\n\tfoo()\n}\nfunc b() {}\n")
	newText := []byte("func a() {\n\tfooBar(1)\n}\nfunc b() {}\n")
	shift := diffText(oldText, newText)
	beforeChange := len("func a() {\n\tfoo")
	if got, ok := shift.toOld(beforeChange); !ok || got != beforeChange {
		test.Errorf("toOld before the change = %d, %v", got, ok)
	}
	newFuncB := len("func a() {\n\tfooBar(1)\n}\nfunc ")
	oldFuncB := len("func a() {\n\tfoo()\n}\nfunc ")
	if got, ok := shift.toOld(newFuncB); !ok || got != oldFuncB {
		test.Errorf("toOld after the change = %d, %v, want %d", got, ok, oldFuncB)
	}
	if got, ok := shift.toNew(oldFuncB); !ok || got != newFuncB {
		test.Errorf("toNew after the change = %d, %v, want %d", got, ok, newFuncB)
	}
	if _, ok := shift.toOld(len("func a() {\n\tfooB")); ok {
		test.Error("toOld inside the change must fail")
	}
	twoChanges := []byte("// new\nfunc a() {\n\tfoo()\n}\nfunc b() {}\nextra\n")
	twoChangeShift := diffText(oldText, twoChanges)
	oldFoo := strings.Index(string(oldText), "foo")
	if got, ok := twoChangeShift.toOld(strings.Index(string(twoChanges), "foo")); !ok || got != oldFoo {
		test.Errorf("toOld between two changes = %d, %v, want %d", got, ok, oldFoo)
	}
	unchangedShift := diffText(oldText, oldText)
	if got, ok := unchangedShift.toOld(5); !ok || got != 5 {
		test.Errorf("toOld with no change = %d, %v", got, ok)
	}
}
