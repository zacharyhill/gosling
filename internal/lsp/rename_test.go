package lsp

import (
	"encoding/json"
	"testing"
)

func editsJSON(test *testing.T, uri string, ranges ...lspRange) json.RawMessage {
	test.Helper()
	edits := make([]textEdit, 0, len(ranges))
	for _, editRange := range ranges {
		edits = append(edits, textEdit{Range: editRange, NewText: "total"})
	}
	raw, err := json.Marshal(map[string]any{
		"documentChanges": []any{map[string]any{
			"textDocument": map[string]any{"uri": uri, "version": 4},
			"edits":        edits,
		}},
	})
	if err != nil {
		test.Fatal(err)
	}
	return raw
}

// firstWord shortens a range that starts with word to only that word.
func firstWord(found lspRange, word string) lspRange {
	end := found.Start
	end.Character += len(word)
	return lspRange{Start: found.Start, End: end}
}

func TestRenameMapsGeneratedEdits(test *testing.T) {
	server, file := newMappingServer(test)
	declaration := firstWord(goRange(test, file, "count,"), "count")
	use := firstWord(goRange(test, file, "count\n"), "count")
	mapped, err := server.mapRename(editsJSON(test, pathToURI(file.goPath), declaration, use, use))
	if err != nil {
		test.Fatal(err)
	}
	if len(mapped.DocumentChanges) != 1 {
		test.Fatalf("got %+v, want one document change", mapped)
	}
	change := mapped.DocumentChanges[0]
	if change.TextDocument.URI != pathToURI(file.golPath) || change.TextDocument.Version != nil {
		test.Errorf("document = %+v, want x.gol without a version", change.TextDocument)
	}
	wantUse := firstWord(golRange(test, "count\n"), "count")
	want := []textEdit{{Range: golRange(test, "count"), NewText: "total"}, {Range: wantUse, NewText: "total"}}
	if len(change.Edits) != len(want) || change.Edits[0] != want[0] || change.Edits[1] != want[1] {
		test.Errorf("edits = %+v, want %+v", change.Edits, want)
	}
}

func TestRenameRejectsGeneratedText(test *testing.T) {
	server, file := newMappingServer(test)
	if _, err := server.mapRename(editsJSON(test, pathToURI(file.goPath), goRange(test, file, "goto"))); err == nil {
		test.Fatal("want an error for an edit in generated code")
	}
}

func TestRenameKeepsPlainGoFiles(test *testing.T) {
	server, _ := newMappingServer(test)
	uri := "file:///work/other/plain.go"
	editRange := lspRange{Start: position{Line: 2, Character: 1}, End: position{Line: 2, Character: 6}}
	mapped, err := server.mapRename(editsJSON(test, uri, editRange))
	if err != nil {
		test.Fatal(err)
	}
	change := mapped.DocumentChanges[0]
	if change.TextDocument.URI != uri || change.TextDocument.Version == nil || *change.TextDocument.Version != 4 {
		test.Errorf("document = %+v, want it unchanged", change.TextDocument)
	}
	if len(change.Edits) != 1 || change.Edits[0].Range != editRange {
		test.Errorf("edits = %+v, want them unchanged", change.Edits)
	}
}

func TestRenameMapsChangesByURI(test *testing.T) {
	server, file := newMappingServer(test)
	use := firstWord(goRange(test, file, "count\n"), "count")
	raw, err := json.Marshal(map[string]any{
		"changes": map[string]any{pathToURI(file.goPath): []textEdit{{Range: use, NewText: "total"}}},
	})
	if err != nil {
		test.Fatal(err)
	}
	mapped, err := server.mapRename(raw)
	if err != nil {
		test.Fatal(err)
	}
	edits, ok := mapped.Changes[pathToURI(file.golPath)]
	if !ok || len(mapped.Changes) != 1 || len(edits) != 1 {
		test.Fatalf("changes = %+v, want one edit in x.gol", mapped.Changes)
	}
}

func TestPrepareRenameMapsRange(test *testing.T) {
	server, file := newMappingServer(test)
	raw, err := json.Marshal(goRange(test, file, "Atoi"))
	if err != nil {
		test.Fatal(err)
	}
	mapped := server.mapPrepareRename(raw, file)
	golAtoi, ok := convertJSON[lspRange](mapped)
	if !ok || golAtoi != golRange(test, "Atoi") {
		test.Errorf("got %v, want %+v", mapped, golRange(test, "Atoi"))
	}
	raw, err = json.Marshal(goRange(test, file, "goto"))
	if err != nil {
		test.Fatal(err)
	}
	if mapped := server.mapPrepareRename(raw, file); mapped != nil {
		test.Errorf("generated range = %v, want nil", mapped)
	}
}
