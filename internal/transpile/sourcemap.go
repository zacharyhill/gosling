package transpile

import (
	"go/token"
	"slices"
	"sort"
)

// maxResyncDistance limits how far token alignment looks ahead when the
// tokens before and after gofmt stop matching.
const maxResyncDistance = 64

// copiedRange is output text that the writer copies from the preprocessed
// source.
type copiedRange struct {
	srcOffset, outOffset, length int
}

// span is a token that the generated file copies from the .gol file.
type span struct {
	golOffset, goOffset, length int
}

// SourceMap maps offsets between a .gol file and its generated .go file. It
// covers each token that the generated file copies from the .gol file.
// Generated text, such as the code that try/catch adds, has no mapping.
type SourceMap struct {
	byGo  []span // sorted by goOffset
	byGol []span // sorted by golOffset
}

// GoOffset maps an offset in the .gol file to the generated file. An offset
// directly after a token maps to the offset directly after the same token.
func (sourceMap SourceMap) GoOffset(golOffset int) (int, bool) {
	found, ok := lookup(sourceMap.byGol, golOffset, func(candidate span) int { return candidate.golOffset })
	if !ok {
		return 0, false
	}
	return found.goOffset + golOffset - found.golOffset, true
}

// GolOffset maps an offset in the generated file to the .gol file.
func (sourceMap SourceMap) GolOffset(goOffset int) (int, bool) {
	found, ok := lookup(sourceMap.byGo, goOffset, func(candidate span) int { return candidate.goOffset })
	if !ok {
		return 0, false
	}
	return found.golOffset + goOffset - found.goOffset, true
}

// GoOffsets returns the generated offsets of the copied tokens that start in
// golStart..golEnd of the .gol file, in .gol order.
func (sourceMap SourceMap) GoOffsets(golStart, golEnd int) []int {
	first := sort.Search(len(sourceMap.byGol), func(spanIndex int) bool { return sourceMap.byGol[spanIndex].golOffset >= golStart })
	var offsets []int
	for _, found := range sourceMap.byGol[first:] {
		if found.golOffset >= golEnd {
			break
		}
		offsets = append(offsets, found.goOffset)
	}
	return offsets
}

// GolOffsetNear maps an offset in the generated file to the .gol file. An
// offset in generated text maps to the end of the nearest copied token
// before it.
func (sourceMap SourceMap) GolOffsetNear(goOffset int) (int, bool) {
	if golOffset, ok := sourceMap.GolOffset(goOffset); ok {
		return golOffset, true
	}
	next := sort.Search(len(sourceMap.byGo), func(spanIndex int) bool { return sourceMap.byGo[spanIndex].goOffset > goOffset })
	if next == 0 {
		return 0, false
	}
	previous := sourceMap.byGo[next-1]
	return previous.golOffset + previous.length, true
}

// lookup finds the span that contains offset. The end of a span counts, so
// a cursor directly after a name still finds the name. When a span ends
// where the next one starts, the next span wins.
func lookup(spans []span, offset int, start func(span) int) (span, bool) {
	next := sort.Search(len(spans), func(spanIndex int) bool { return start(spans[spanIndex]) > offset })
	if next == 0 {
		return span{}, false
	}
	found := spans[next-1]
	if offset > start(found)+found.length {
		return span{}, false
	}
	return found, true
}

type scannedToken struct {
	offset int
	text   string
}

// buildSourceMap pairs the tokens of the unformatted output with the tokens
// of the final output. gofmt changes only the space between tokens, so the
// token sequences match. Each part of a token that the writer copied from
// the preprocessed source, and that preprocessing did not change, gets a
// span. So ColorRed, made from Red, maps its last three bytes.
func buildSourceMap(file *file, unformatted []byte, copies []copiedRange, formatted []byte) SourceMap {
	var sourceMap SourceMap
	for _, pair := range alignTokens(scanTokens(unformatted), scanTokens(formatted)) {
		for _, piece := range copiedPieces(file, copies, pair[0]) {
			piece.goOffset += pair[1].offset
			sourceMap.byGo = append(sourceMap.byGo, piece)
		}
	}
	sourceMap.byGol = slices.Clone(sourceMap.byGo)
	slices.SortFunc(sourceMap.byGol, func(left, right span) int { return left.golOffset - right.golOffset })
	return sourceMap
}

// copiedPieces returns a span for each part of a token in the unformatted
// output that comes unchanged from the .gol file. The goOffset of each span
// is relative to the start of the token.
func copiedPieces(file *file, copies []copiedRange, unformattedToken scannedToken) []span {
	tokenEnd := unformattedToken.offset + len(unformattedToken.text)
	first := sort.Search(len(copies), func(copyIndex int) bool {
		return copies[copyIndex].outOffset+copies[copyIndex].length > unformattedToken.offset
	})
	var pieces []span
	for copyIndex := first; copyIndex < len(copies) && copies[copyIndex].outOffset < tokenEnd; copyIndex++ {
		copied := copies[copyIndex]
		pieceStart, pieceEnd := max(copied.outOffset, unformattedToken.offset), min(copied.outOffset+copied.length, tokenEnd)
		if pieceStart >= pieceEnd {
			continue
		}
		srcStart := copied.srcOffset + pieceStart - copied.outOffset
		srcEnd := srcStart + pieceEnd - pieceStart
		for _, origPiece := range file.preprocessed.OrigPieces(srcStart, srcEnd) {
			tokenOffset := pieceStart - unformattedToken.offset + origPiece.Start - srcStart
			pieceText := unformattedToken.text[tokenOffset : tokenOffset+origPiece.End-origPiece.Start]
			golEnd := min(origPiece.OrigStart+len(pieceText), len(file.original))
			if string(file.original[origPiece.OrigStart:golEnd]) == pieceText {
				pieces = append(pieces, span{golOffset: origPiece.OrigStart, goOffset: tokenOffset, length: len(pieceText)})
			}
		}
	}
	return pieces
}

// scanTokens returns the tokens that gofmt keeps. gofmt adds and removes
// semicolons and parentheses, so scanTokens skips them.
func scanTokens(src []byte) []scannedToken {
	sourceScanner, tokenFile := newScanner(src, 0)
	var tokens []scannedToken
	for {
		tokenPosition, kind, literal := sourceScanner.Scan()
		switch kind {
		case token.EOF:
			return tokens
		case token.SEMICOLON, token.LPAREN, token.RPAREN:
			continue
		}
		text := literal
		if text == "" {
			text = kind.String()
		}
		tokens = append(tokens, scannedToken{offset: tokenFile.Offset(tokenPosition), text: text})
	}
}

// alignTokens pairs equal tokens of before and after in order. gofmt sorts
// imports, so after a mismatch it looks ahead for the next match.
func alignTokens(before, after []scannedToken) [][2]scannedToken {
	var pairs [][2]scannedToken
	beforeIndex, afterIndex := 0, 0
	for beforeIndex < len(before) && afterIndex < len(after) {
		if before[beforeIndex].text == after[afterIndex].text {
			pairs = append(pairs, [2]scannedToken{before[beforeIndex], after[afterIndex]})
			beforeIndex++
			afterIndex++
			continue
		}
		beforeSkip, afterSkip := resync(before[beforeIndex:], after[afterIndex:])
		beforeIndex += beforeSkip
		afterIndex += afterSkip
	}
	return pairs
}

// resync returns how many tokens to skip in each sequence to reach the
// nearest pair of equal tokens.
func resync(before, after []scannedToken) (int, int) {
	for distance := 1; distance <= maxResyncDistance; distance++ {
		for beforeSkip := 0; beforeSkip <= distance; beforeSkip++ {
			afterSkip := distance - beforeSkip
			if beforeSkip < len(before) && afterSkip < len(after) && before[beforeSkip].text == after[afterSkip].text {
				return beforeSkip, afterSkip
			}
		}
	}
	return 1, 1
}
