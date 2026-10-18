package lsp

import (
	"errors"
	"go/scanner"
	"slices"

	"github.com/zacharyhill/gosling/internal/transpile"
)

// maxHunks limits the changed regions that buildParseableVersion tries
// one at a time. Each try parses the file again.
const maxHunks = 64

// hunk is a changed region of whole lines between two versions of a text.
type hunk struct {
	oldStart, oldEnd, newStart, newEnd int
}

// findLineHunks returns the changed regions between two texts, in order.
func findLineHunks(oldText, newText []byte) []hunk {
	var hunks []hunk
	oldOffset, newOffset := 0, 0
	for _, run := range append(matchLineRuns(oldText, newText), matchedRun{oldStart: len(oldText), newStart: len(newText)}) {
		if run.oldStart > oldOffset || run.newStart > newOffset {
			hunks = append(hunks, hunk{oldStart: oldOffset, oldEnd: run.oldStart, newStart: newOffset, newEnd: run.newStart})
		}
		oldOffset, newOffset = run.oldStart+run.length, run.newStart+run.length
	}
	return hunks
}

// applyHunks returns oldText with the new text of each hunk in hunks.
func applyHunks(oldText, newText []byte, hunks []hunk) []byte {
	var merged []byte
	copiedUpTo := 0
	for _, changed := range hunks {
		merged = append(merged, oldText[copiedUpTo:changed.oldStart]...)
		merged = append(merged, newText[changed.newStart:changed.newEnd]...)
		copiedUpTo = changed.oldEnd
	}
	return append(merged, oldText[copiedUpTo:]...)
}

// buildParseableVersion returns a version of input that parses. It starts from
// parsed, the last version that parsed, and adds the changed lines of input
// that keep it parseable. First it adds all changes without errors from
// parseErrors. Then it tries each other change alone.
func buildParseableVersion(input transpile.Input, parsed []byte, parseErrors error) []byte {
	hunks := findLineHunks(parsed, input.Src)
	if len(hunks) > maxHunks {
		return parsed
	}
	isParseable := func(chosen []hunk) bool {
		text := applyHunks(parsed, input.Src, chosen)
		return transpile.FindParseErrors(transpile.Input{Path: input.Path, Src: text}) == nil
	}
	errorOffsets := findErrorLineStarts(input.Src, parseErrors)
	hasError := func(changed hunk) bool {
		return slices.ContainsFunc(errorOffsets, func(offset int) bool {
			return changed.newStart <= offset && offset <= changed.newEnd
		})
	}
	keptHunks := slices.DeleteFunc(slices.Clone(hunks), hasError)
	if !isParseable(keptHunks) {
		keptHunks = nil
	}
	for _, changed := range hunks {
		if slices.Contains(keptHunks, changed) {
			continue
		}
		candidate := append(slices.Clone(keptHunks), changed)
		slices.SortFunc(candidate, func(left, right hunk) int { return left.oldStart - right.oldStart })
		if isParseable(candidate) {
			keptHunks = candidate
		}
	}
	return applyHunks(parsed, input.Src, keptHunks)
}

// findErrorLineStarts returns the offset of the line of each error in text.
func findErrorLineStarts(text []byte, err error) []int {
	var offsets []int
	for _, problem := range splitErrors(err) {
		var positioned *scanner.Error
		if !errors.As(problem, &positioned) {
			continue
		}
		if offset, ok := offsetAt(text, position{Line: positioned.Pos.Line - 1}); ok {
			offsets = append(offsets, offset)
		}
	}
	return offsets
}
