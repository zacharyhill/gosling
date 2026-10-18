package lsp

import (
	"bytes"
	"go/scanner"
	"go/token"
	"path/filepath"
	"slices"
	"sort"

	"github.com/zacharyhill/gosling/internal/transpile"
)

// mapGeneratedEdits maps the edits that gopls makes in generated text. Such
// text often holds a name that gosling copies from gosling syntax, such as
// a catch variable or an arrow parameter. Its origin is the same name in
// the .gol file, in text that has no copied token. The rename edits these
// origins and drops the generated edits. It then transpiles the edited .gol
// files. The rename is valid only if the result has the tokens of the gopls
// result.
func (server *server) mapGeneratedEdits(renames []*fileRename) error {
	var renamesWithGenerated []*fileRename
	for _, rename := range renames {
		if len(rename.generatedEdits) == 0 {
			continue
		}
		if !bytes.Equal(server.currentGolText(rename.file), rename.file.golText) {
			return errRenameGenerated
		}
		renamesWithGenerated = append(renamesWithGenerated, rename)
	}
	if len(renamesWithGenerated) == 0 {
		return nil
	}
	// The first try renames each origin that declares a copied name. A
	// field name in a destructuring pattern has no copied name after it,
	// so the second try renames all origins.
	for _, renameAll := range []bool{false, true} {
		origins, ok := buildAllOriginEdits(renamesWithGenerated, renameAll)
		if ok && server.isRenameConsistent(renames, origins) {
			for rename, edits := range origins {
				for _, edit := range edits {
					rename.addEdit(edit)
				}
			}
			return nil
		}
	}
	return errRenameGenerated
}

func buildAllOriginEdits(renames []*fileRename, renameAll bool) (map[*fileRename][]textEdit, bool) {
	origins := map[*fileRename][]textEdit{}
	for _, rename := range renames {
		edits, ok := buildOriginEdits(rename, renameAll)
		if !ok {
			return nil, false
		}
		origins[rename] = edits
	}
	return origins, true
}

// buildOriginEdits returns the edits of the origins of the generated edits. All
// generated edits must replace the same name with the same text.
func buildOriginEdits(rename *fileRename, renameAll bool) ([]textEdit, bool) {
	file := rename.file
	oldName, newName, ok := findRenamedName(file, rename.generatedEdits)
	if !ok {
		return nil, false
	}
	candidates := findUncopiedNames(file, oldName)
	if !renameAll {
		candidates = filterDeclaringNames(candidates, getEditOffsets(file.golText, rename.edits))
	}
	if len(candidates) == 0 {
		return nil, false
	}
	edits := make([]textEdit, 0, len(candidates))
	for _, candidate := range candidates {
		editRange := lspRange{Start: positionAt(file.golText, candidate.start), End: positionAt(file.golText, candidate.end)}
		edits = append(edits, textEdit{Range: editRange, NewText: newName})
	}
	return edits, true
}

func findRenamedName(file *generatedFile, edits []textEdit) (string, string, bool) {
	oldName, newName := "", edits[0].NewText
	for _, edit := range edits {
		start, startOK := offsetAt(file.goText, edit.Range.Start)
		end, endOK := offsetAt(file.goText, edit.Range.End)
		if !startOK || !endOK || end <= start || edit.NewText != newName {
			return "", "", false
		}
		name := string(file.goText[start:end])
		if oldName != "" && name != oldName {
			return "", "", false
		}
		oldName = name
	}
	return oldName, newName, true
}

// findUncopiedNames returns the identifiers called name that the generated
// file does not copy.
func findUncopiedNames(file *generatedFile, name string) []golToken {
	var found []golToken
	for _, candidate := range scanGolTokens(file.golText) {
		if candidate.kind == token.IDENT && candidate.text == name && len(file.sourceMap.GoOffsets(candidate.start, candidate.start+1)) == 0 {
			found = append(found, candidate)
		}
	}
	return found
}

// filterDeclaringNames returns the candidates that declare a copied name. A
// declaration comes before its uses, so each copied name belongs to the
// nearest candidate before it.
func filterDeclaringNames(candidates []golToken, copiedOffsets []int) []golToken {
	declaring := map[int]bool{}
	for _, offset := range copiedOffsets {
		next := sort.Search(len(candidates), func(candidateIndex int) bool { return candidates[candidateIndex].start >= offset })
		if next > 0 {
			declaring[next-1] = true
		}
	}
	var found []golToken
	for candidateIndex, candidate := range candidates {
		if declaring[candidateIndex] {
			found = append(found, candidate)
		}
	}
	return found
}

func getEditOffsets(text []byte, edits []textEdit) []int {
	offsets := make([]int, 0, len(edits))
	for _, edit := range edits {
		if offset, ok := offsetAt(text, edit.Range.Start); ok {
			offsets = append(offsets, offset)
		}
	}
	return offsets
}

// isRenameConsistent transpiles each package with the renamed .gol files.
// It reports whether each generated file gets the tokens that gopls gives
// it. origins holds the extra edits of some files.
func (server *server) isRenameConsistent(renames []*fileRename, origins map[*fileRename][]textEdit) bool {
	renamesByDir := map[string][]*fileRename{}
	for _, rename := range renames {
		if rename.file != nil {
			dir := filepath.Dir(rename.file.golPath)
			renamesByDir[dir] = append(renamesByDir[dir], rename)
		}
	}
	for dir, dirRenames := range renamesByDir {
		if !server.isPackageRenameConsistent(dir, dirRenames, origins) {
			return false
		}
	}
	return true
}

func (server *server) isPackageRenameConsistent(dir string, renames []*fileRename, origins map[*fileRename][]textEdit) bool {
	sources := map[string][]byte{}
	for _, document := range server.documentsIn(dir) {
		sources[document.path] = document.text
	}
	for _, rename := range renames {
		renamed, ok := applyEdits(server.currentGolText(rename.file), append(slices.Clone(rename.edits), origins[rename]...))
		if !ok {
			return false
		}
		sources[rename.file.golPath] = renamed
	}
	// Open packages have no line directives. Other packages match the files
	// on disk, which have them.
	_, isOpen := server.packages[dir]
	outputs, err := server.generator.Generate(dir, sources, !isOpen)
	if err != nil {
		return false
	}
	for _, rename := range renames {
		want, ok := applyEdits(rename.file.goText, rename.goEdits)
		if !ok || !hasMatchingOutput(outputs, rename.file.goPath, want) {
			return false
		}
	}
	return true
}

func hasMatchingOutput(outputs []transpile.Output, goPath string, want []byte) bool {
	for _, output := range outputs {
		if output.Path == goPath {
			return hasSameGoTokens(output.Src, want)
		}
	}
	return false
}

// applyEdits applies edits that do not overlap. Equal edits count once.
func applyEdits(text []byte, edits []textEdit) ([]byte, bool) {
	type offsetEdit struct {
		start, end int
		text       string
	}
	var offsetEdits []offsetEdit
	for _, edit := range edits {
		start, startOK := offsetAt(text, edit.Range.Start)
		end, endOK := offsetAt(text, edit.Range.End)
		if !startOK || !endOK || end < start {
			return nil, false
		}
		converted := offsetEdit{start: start, end: end, text: edit.NewText}
		if !slices.Contains(offsetEdits, converted) {
			offsetEdits = append(offsetEdits, converted)
		}
	}
	slices.SortFunc(offsetEdits, func(left, right offsetEdit) int { return left.start - right.start })
	var edited []byte
	copiedUpTo := 0
	for _, edit := range offsetEdits {
		if edit.start < copiedUpTo {
			return nil, false
		}
		edited = append(edited, text[copiedUpTo:edit.start]...)
		edited = append(edited, edit.text...)
		copiedUpTo = edit.end
	}
	return append(edited, text[copiedUpTo:]...), true
}

// hasSameGoTokens reports whether two Go files have the same tokens and
// comments. gofmt can align a renamed file in a different way.
func hasSameGoTokens(left, right []byte) bool {
	return slices.Equal(scanGoTokens(left), scanGoTokens(right))
}

type goToken struct {
	kind    token.Token
	literal string
}

func scanGoTokens(src []byte) []goToken {
	tokenFile := token.NewFileSet().AddFile("", -1, len(src))
	var goScanner scanner.Scanner
	goScanner.Init(tokenFile, src, nil, scanner.ScanComments)
	var tokens []goToken
	for {
		_, kind, literal := goScanner.Scan()
		if kind == token.EOF {
			return tokens
		}
		tokens = append(tokens, goToken{kind: kind, literal: literal})
	}
}

// findRenameTarget handles a rename at a name that the generated file does not
// copy, such as a catch variable. It returns the position of the first
// copied use of the name in the generated file, and the range of the name.
// gopls can rename at the use.
func (server *server) findRenameTarget(file *generatedFile, golPosition position) (position, lspRange, bool) {
	currentText := server.currentGolText(file)
	offset, ok := offsetAt(currentText, golPosition)
	if !ok || !bytes.Equal(currentText, file.golText) {
		return position{}, lspRange{}, false
	}
	tokens := scanGolTokens(file.golText)
	nameIndex := slices.IndexFunc(tokens, func(candidate golToken) bool {
		return candidate.kind == token.IDENT && candidate.start <= offset && offset <= candidate.end
	})
	if nameIndex < 0 {
		return position{}, lspRange{}, false
	}
	name := tokens[nameIndex]
	for _, later := range tokens[nameIndex+1:] {
		if later.kind != token.IDENT || later.text != name.text {
			continue
		}
		goOffsets := file.sourceMap.GoOffsets(later.start, later.start+1)
		if len(goOffsets) == 0 {
			// Another declaration of the name starts here.
			return position{}, lspRange{}, false
		}
		nameRange := lspRange{Start: positionAt(currentText, name.start), End: positionAt(currentText, name.end)}
		return positionAt(file.goText, goOffsets[0]), nameRange, true
	}
	return position{}, lspRange{}, false
}
