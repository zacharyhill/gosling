package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zacharyhill/gosling/internal/lsp"
	"github.com/zacharyhill/gosling/internal/transpile"
)

// lspTimeout is long because gopls loads the module and the standard
// library on its first request.
const lspTimeout = 60 * time.Second

const lspMain = `package main

import (
	"fmt"
	"os"
)

func load(path: string): (string, error) {
	try {
		const data = os.ReadFile(path)
		return string(data), nil
	} catch (e) {
		return "", fmt.Errorf("load: %w", e)
	}
}

func main() {
	(&counter{}).bump()
	try {
		const text = load("config.txt")
		fmt.Println(t` + "`got ${text} ${helperName()}`" + `)
	} catch (e) {
		fmt.Println(e)
	}
}
`

const lspHelper = `package main

func helperName() string {
	const name = "helper"
	return name
}

type counter struct{ n int }

impl counter {
	func bump() int {
		this.n++
		return this.n
	}
}
`

type lspMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

// testClient drives the language server like an editor. It answers the
// requests that gopls sends to the editor.
type testClient struct {
	test          *testing.T
	writer        io.Writer
	messages      chan lspMessage
	nextID        int
	notifications []lspMessage
	// initializeResult is the response to initialize. It lists the server
	// capabilities.
	initializeResult json.RawMessage
}

func startLSP(test *testing.T, dir string) (*testClient, <-chan error) {
	test.Helper()
	goplsPath, err := exec.LookPath("gopls")
	if err != nil {
		test.Skip("gopls is not installed")
	}
	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	test.Cleanup(cancel)
	served := make(chan error, 1)
	go func() {
		options := lsp.Options{Generator: generator{}, GoplsPath: goplsPath, GenerateOnSave: true, Logger: log.New(io.Discard, "", 0)}
		serveErr := lsp.Serve(ctx, serverReader, serverWriter, options)
		served <- errors.Join(serveErr, serverWriter.Close())
	}()
	test.Cleanup(func() {
		if err := clientWriter.Close(); err != nil {
			test.Errorf("close client pipe: %v", err)
		}
	})
	client := &testClient{test: test, writer: clientWriter, messages: make(chan lspMessage, 100)}
	go client.readLoop(bufio.NewReader(clientReader))
	client.initializeResult = client.request("initialize", map[string]any{
		"processId":        nil,
		"rootUri":          fileURI(dir),
		"capabilities":     map[string]any{},
		"workspaceFolders": []any{map[string]any{"uri": fileURI(dir), "name": "app"}},
	})
	client.notify("initialized", map[string]any{})
	return client, served
}

func fileURI(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
}

func (client *testClient) readLoop(reader *bufio.Reader) {
	defer close(client.messages)
	for {
		length, err := readContentLength(reader)
		if err != nil {
			return
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(reader, body); err != nil {
			return
		}
		var message lspMessage
		if json.Unmarshal(body, &message) == nil {
			client.messages <- message
		}
	}
}

// readContentLength reads the header lines of one message.
func readContentLength(reader *bufio.Reader) (int, error) {
	var length int
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return 0, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			return length, nil
		}
		if value, ok := strings.CutPrefix(line, "Content-Length: "); ok {
			if _, err := fmt.Sscan(value, &length); err != nil {
				return 0, err
			}
		}
	}
}

func (client *testClient) send(message map[string]any) {
	client.test.Helper()
	message["jsonrpc"] = "2.0"
	body, err := json.Marshal(message)
	if err != nil {
		client.test.Fatal(err)
	}
	if _, err := fmt.Fprintf(client.writer, "Content-Length: %d\r\n\r\n%s", len(body), body); err != nil {
		client.test.Fatal(err)
	}
}

func (client *testClient) notify(method string, params any) {
	client.test.Helper()
	client.send(map[string]any{"method": method, "params": params})
}

// request sends a request and returns its result. It fails the test if the
// server returns an error.
func (client *testClient) request(method string, params any) json.RawMessage {
	client.test.Helper()
	response := client.call(method, params)
	if response.Error != nil {
		client.test.Fatalf("%s failed: %s", method, response.Error)
	}
	return response.Result
}

// call sends a request and returns the response. It answers server requests
// and keeps notifications while it waits.
func (client *testClient) call(method string, params any) lspMessage {
	client.test.Helper()
	client.nextID++
	id := fmt.Sprint(client.nextID)
	client.send(map[string]any{"id": client.nextID, "method": method, "params": params})
	for {
		message := client.next()
		if message.Method == "" && string(message.ID) == id {
			return message
		}
	}
}

// next returns the next message that is not a server request.
func (client *testClient) next() lspMessage {
	client.test.Helper()
	timeout := time.After(lspTimeout)
	for {
		select {
		case message, ok := <-client.messages:
			if !ok {
				client.test.Fatal("server closed the connection")
			}
			if message.Method != "" && message.ID != nil {
				client.answer(message)
				continue
			}
			if message.Method != "" {
				client.notifications = append(client.notifications, message)
			}
			return message
		case <-timeout:
			client.test.Fatal("timed out waiting for the server")
		}
	}
}

func (client *testClient) answer(message lspMessage) {
	client.test.Helper()
	var result any
	if message.Method == "workspace/configuration" {
		var params struct {
			Items []any `json:"items"`
		}
		if err := json.Unmarshal(message.Params, &params); err != nil {
			client.test.Fatal(err)
		}
		result = make([]any, len(params.Items))
	}
	client.send(map[string]any{"id": message.ID, "result": result})
}

// waitForDiagnostics returns the next diagnostics for uri that satisfy want.
func (client *testClient) waitForDiagnostics(uri string, want func([]lspDiagnostic) bool) []lspDiagnostic {
	client.test.Helper()
	matchDiagnostics := func(message lspMessage) ([]lspDiagnostic, bool) {
		var params struct {
			URI         string          `json:"uri"`
			Diagnostics []lspDiagnostic `json:"diagnostics"`
		}
		if message.Method != "textDocument/publishDiagnostics" || json.Unmarshal(message.Params, &params) != nil || params.URI != uri {
			return nil, false
		}
		return params.Diagnostics, want(params.Diagnostics)
	}
	for _, message := range client.notifications {
		if diagnostics, ok := matchDiagnostics(message); ok {
			return diagnostics
		}
	}
	for {
		if diagnostics, ok := matchDiagnostics(client.next()); ok {
			return diagnostics
		}
	}
}

type lspPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type lspLocation struct {
	URI   string `json:"uri"`
	Range struct {
		Start lspPosition `json:"start"`
	} `json:"range"`
}

type lspDiagnostic struct {
	Message string `json:"message"`
	Source  string `json:"source"`
}

// positionOf returns the position of the first occurrence of needle in
// text. The texts in these tests are ASCII, so bytes equal UTF-16 units.
func positionOf(test *testing.T, text, needle string) lspPosition {
	test.Helper()
	offset := strings.Index(text, needle)
	if offset < 0 {
		test.Fatalf("%q not found", needle)
	}
	lineStart := strings.LastIndex(text[:offset], "\n") + 1
	return lspPosition{Line: strings.Count(text[:offset], "\n"), Character: offset - lineStart}
}

func textDocumentPosition(uri string, cursor lspPosition) map[string]any {
	return map[string]any{"textDocument": map[string]any{"uri": uri}, "position": cursor}
}

func decodeLocations(test *testing.T, raw json.RawMessage) []lspLocation {
	test.Helper()
	var locations []lspLocation
	if err := json.Unmarshal(raw, &locations); err != nil {
		test.Fatalf("decode %s: %v", raw, err)
	}
	return locations
}

// lspSession is a running server with main.gol open in the editor.
type lspSession struct {
	client  *testClient
	dir     string
	mainURI string
	version int
}

// changeMain replaces the editor text of main.gol.
func (session *lspSession) changeMain(text string) {
	session.version++
	session.client.notify("textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": session.mainURI, "version": session.version},
		"contentChanges": []any{map[string]any{"text": text}},
	})
}

// waitUntilHandled sends a hover and waits for the response. The server
// handles messages in order, so the earlier notifications are then done.
func (session *lspSession) waitUntilHandled(test *testing.T) {
	test.Helper()
	session.client.request("textDocument/hover", textDocumentPosition(session.mainURI, positionOf(test, lspMain, "ReadFile")))
}

func (session *lspSession) save(test *testing.T) {
	test.Helper()
	session.client.notify("textDocument/didSave", map[string]any{"textDocument": map[string]any{"uri": session.mainURI}})
	session.waitUntilHandled(test)
}

func (session *lspSession) rename(test *testing.T, needle, newName string) lspMessage {
	test.Helper()
	params := textDocumentPosition(session.mainURI, positionOf(test, lspMain, needle))
	params["newName"] = newName
	return session.client.call("textDocument/rename", params)
}

func (session *lspSession) definition(test *testing.T, text, needle string) []lspLocation {
	test.Helper()
	params := textDocumentPosition(session.mainURI, positionOf(test, text, needle))
	return decodeLocations(test, session.client.request("textDocument/definition", params))
}

func TestLSP(test *testing.T) {
	if testing.Short() {
		test.Skip("runs gopls")
	}
	dir := test.TempDir()
	writeTree(test, dir, map[string]string{
		"go.mod":     goModFile,
		"main.gol":   lspMain,
		"helper.gol": lspHelper,
	})
	client, served := startLSP(test, dir)
	session := &lspSession{client: client, dir: dir, mainURI: fileURI(filepath.Join(dir, "main.gol")), version: 1}
	client.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{"uri": session.mainURI, "languageId": "gol", "version": 1, "text": lspMain},
	})
	test.Run("definition in the same file", session.checkSameFileDefinition)
	test.Run("definition in another gol file", session.checkOtherFileDefinition)
	test.Run("definition of an annotated parameter", session.checkAnnotatedParameterDefinition)
	test.Run("hover on a standard library call", session.checkHover)
	test.Run("position in generated code has no result", session.checkGeneratedPosition)
	test.Run("hover on gosling syntax shows the generated code", session.checkGeneratedHover)
	test.Run("rename across gol files", session.checkRename)
	test.Run("rename of a name in generated code", session.checkRenameInGeneratedCode)
	test.Run("rename at a catch variable", session.checkRenameAtCatchVariable)
	test.Run("rename of an impl method", session.checkImplMethodRename)
	test.Run("outline lists impl methods", session.checkImplOutline)
	test.Run("save writes generated files", session.checkGenerateOnSave)
	test.Run("save with errors writes nothing", session.checkSaveWithErrors)
	test.Run("server asks for gol file renames and deletes", session.checkFileOperationCapability)
	test.Run("renamed gol file loses its old output", session.checkFileRename)
	test.Run("deleted gol file loses its output", session.checkFileDelete)
	test.Run("syntax error becomes a diagnostic", session.checkSyntaxError)
	test.Run("edit after the last good version still maps", session.checkStaleMapping)
	test.Run("changed lines that parse work despite a syntax error", session.checkChangedLinesWithSyntaxError)
	test.Run("completion in code with a type error", session.checkCompletion)
	test.Run("arrow parameters map to the gol file", session.checkArrowParams)
	test.Run("format a gol document", session.checkFormatting)
	client.request("shutdown", nil)
	client.notify("exit", nil)
	select {
	case err := <-served:
		if err != nil {
			test.Fatalf("Serve: %v", err)
		}
	case <-time.After(lspTimeout):
		test.Fatal("Serve did not return after exit")
	}
}

func (session *lspSession) checkSameFileDefinition(test *testing.T) {
	locations := session.definition(test, lspMain, "load(\"config")
	want := positionOf(test, lspMain, "load(path")
	if len(locations) != 1 || locations[0].URI != session.mainURI || locations[0].Range.Start != want {
		test.Errorf("got %+v, want %s at %+v", locations, session.mainURI, want)
	}
}

func (session *lspSession) checkAnnotatedParameterDefinition(test *testing.T) {
	locations := session.definition(test, lspMain, "path)")
	want := positionOf(test, lspMain, "path: string")
	if len(locations) != 1 || locations[0].URI != session.mainURI || locations[0].Range.Start != want {
		test.Errorf("got %+v, want %s at %+v", locations, session.mainURI, want)
	}
}

func (session *lspSession) checkOtherFileDefinition(test *testing.T) {
	locations := session.definition(test, lspMain, "helperName")
	helperURI := fileURI(filepath.Join(session.dir, "helper.gol"))
	want := positionOf(test, lspHelper, "helperName")
	if len(locations) != 1 || locations[0].URI != helperURI || locations[0].Range.Start != want {
		test.Errorf("got %+v, want %s at %+v", locations, helperURI, want)
	}
}

func (session *lspSession) checkHover(test *testing.T) {
	hover := session.client.request("textDocument/hover", textDocumentPosition(session.mainURI, positionOf(test, lspMain, "ReadFile")))
	if !strings.Contains(string(hover), "ReadFile") {
		test.Errorf("hover = %s, want ReadFile documentation", hover)
	}
}

func (session *lspSession) checkGeneratedPosition(test *testing.T) {
	definition := session.client.request("textDocument/definition", textDocumentPosition(session.mainURI, positionOf(test, lspMain, "try")))
	if string(definition) != "null" {
		test.Errorf("definition of try = %s, want null", definition)
	}
}

func (session *lspSession) checkGeneratedHover(test *testing.T) {
	hover := session.client.request("textDocument/hover", textDocumentPosition(session.mainURI, positionOf(test, lspMain, "try")))
	if !strings.Contains(string(hover), "goto catch_1") {
		test.Errorf("hover on try = %s, want the generated code", hover)
	}
}

func (session *lspSession) checkSyntaxError(test *testing.T) {
	session.changeMain(strings.Replace(lspMain, "return string(data), nil", "return string(data), nil +", 1))
	session.client.waitForDiagnostics(session.mainURI, func(diagnostics []lspDiagnostic) bool {
		return len(diagnostics) > 0 && diagnostics[0].Source == "gosling"
	})
	session.changeMain(lspMain)
	session.client.waitForDiagnostics(session.mainURI, func(diagnostics []lspDiagnostic) bool { return len(diagnostics) == 0 })
}

// checkStaleMapping edits two places and leaves a syntax error, so the
// server must map through the last version that parsed.
func (session *lspSession) checkStaleMapping(test *testing.T) {
	edited := strings.Replace(lspMain, "func main() {", "// A comment above main.\nfunc main() {", 1)
	edited = strings.Replace(edited, "fmt.Println(e)\n\t}\n}", "fmt.Println(e)\n\t}\n\tundefinedName(\n}", 1)
	session.changeMain(edited)
	locations := session.definition(test, edited, "load(\"config")
	want := positionOf(test, edited, "load(path")
	if len(locations) != 1 || locations[0].Range.Start != want {
		test.Errorf("got %+v, want %+v", locations, want)
	}
}

// checkChangedLinesWithSyntaxError adds a function and a call to it, and
// leaves a syntax error in another function. The new lines parse, so
// definition works on them.
func (session *lspSession) checkChangedLinesWithSyntaxError(test *testing.T) {
	edited := strings.Replace(lspMain, "func main() {", "func extra() string {\n\treturn \"extra\"\n}\n\nfunc main() {\n\tfmt.Println(extra())", 1)
	edited = strings.Replace(edited, "return string(data), nil", "return string(data), nil +", 1)
	session.changeMain(edited)
	session.client.waitForDiagnostics(session.mainURI, func(diagnostics []lspDiagnostic) bool {
		return len(diagnostics) > 0 && diagnostics[0].Source == "gosling"
	})
	locations := session.definition(test, edited, "extra())")
	want := positionOf(test, edited, "extra() string")
	if len(locations) != 1 || locations[0].URI != session.mainURI || locations[0].Range.Start != want {
		test.Errorf("got %+v, want %+v", locations, want)
	}
	session.changeMain(lspMain)
}

func (session *lspSession) checkCompletion(test *testing.T) {
	edited := strings.Replace(lspMain, "\t\tfmt.Println(e)\n", "\t\tfmt.Println(e)\n\t\tfmt.Pr\n", 1)
	session.changeMain(edited)
	cursor := positionOf(test, edited, "fmt.Pr\n")
	cursor.Character += len("fmt.Pr")
	var completion struct {
		Items []struct {
			Label string `json:"label"`
		} `json:"items"`
	}
	raw := session.client.request("textDocument/completion", textDocumentPosition(session.mainURI, cursor))
	if err := json.Unmarshal(raw, &completion); err != nil {
		test.Fatalf("decode %s: %v", raw, err)
	}
	labels := make([]string, 0, len(completion.Items))
	for _, completionItem := range completion.Items {
		labels = append(labels, completionItem.Label)
	}
	if !slices.Contains(labels, "Println") {
		test.Errorf("completion labels = %v, want Println", labels)
	}
}

func (session *lspSession) checkArrowParams(test *testing.T) {
	edited := lspMain + "\nfunc doubled(counts []int) []int {\n\treturn counts.map(count => count * 2)\n}\n"
	session.changeMain(edited)
	locations := session.definition(test, edited, "count * 2")
	want := positionOf(test, edited, "count =>")
	if len(locations) != 1 || locations[0].Range.Start != want {
		test.Errorf("definition = %+v, want %+v", locations, want)
	}
	hover := session.client.request("textDocument/hover", textDocumentPosition(session.mainURI, want))
	if !strings.Contains(string(hover), "count int") {
		test.Errorf("hover on the parameter = %s, want its type", hover)
	}
}

// editCountsByURI returns the number of edits for each URI in a rename result.
func editCountsByURI(test *testing.T, raw json.RawMessage) map[string]int {
	test.Helper()
	var workspaceEdit struct {
		Changes         map[string][]json.RawMessage `json:"changes"`
		DocumentChanges []struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
			Edits []json.RawMessage `json:"edits"`
		} `json:"documentChanges"`
	}
	if err := json.Unmarshal(raw, &workspaceEdit); err != nil {
		test.Fatalf("decode %s: %v", raw, err)
	}
	counts := map[string]int{}
	for uri, edits := range workspaceEdit.Changes {
		counts[uri] += len(edits)
	}
	for _, change := range workspaceEdit.DocumentChanges {
		counts[change.TextDocument.URI] += len(change.Edits)
	}
	return counts
}

func (session *lspSession) checkRename(test *testing.T) {
	response := session.rename(test, "helperName", "helperText")
	if response.Error != nil {
		test.Fatalf("rename failed: %s", response.Error)
	}
	counts := editCountsByURI(test, response.Result)
	helperURI := fileURI(filepath.Join(session.dir, "helper.gol"))
	if len(counts) != 2 || counts[session.mainURI] != 1 || counts[helperURI] != 1 {
		test.Errorf("edits by file = %v, want one in main.gol and one in helper.gol", counts)
	}
}

// checkRenameInGeneratedCode renames a catch variable. gosling writes its
// declaration, so the rename edits the name in the catch clause. The other
// catch variable keeps its name.
func (session *lspSession) checkRenameInGeneratedCode(test *testing.T) {
	response := session.rename(test, "e)\n\t}\n}\n\nfunc main", "failure")
	if response.Error != nil {
		test.Fatalf("rename failed: %s", response.Error)
	}
	counts := editCountsByURI(test, response.Result)
	if len(counts) != 1 || counts[session.mainURI] != 2 {
		test.Errorf("edits by file = %v, want two in main.gol", counts)
	}
}

// checkRenameAtCatchVariable starts a rename at the name in the catch clause.
// The generated file has no copy of that name.
func (session *lspSession) checkRenameAtCatchVariable(test *testing.T) {
	binding := positionOf(test, lspMain, "e) {")
	result := session.client.request("textDocument/prepareRename", textDocumentPosition(session.mainURI, binding))
	var prepared struct {
		Start lspPosition `json:"start"`
	}
	if err := json.Unmarshal(result, &prepared); err != nil || prepared.Start != binding {
		test.Fatalf("prepareRename = %s, want the range of the catch variable", result)
	}
	params := textDocumentPosition(session.mainURI, binding)
	params["newName"] = "failure"
	counts := editCountsByURI(test, session.client.request("textDocument/rename", params))
	if len(counts) != 1 || counts[session.mainURI] != 2 {
		test.Errorf("edits by file = %v, want two in main.gol", counts)
	}
}

func (session *lspSession) checkImplMethodRename(test *testing.T) {
	response := session.rename(test, "bump", "increment")
	if response.Error != nil {
		test.Fatalf("rename failed: %s", response.Error)
	}
	counts := editCountsByURI(test, response.Result)
	helperURI := fileURI(filepath.Join(session.dir, "helper.gol"))
	if len(counts) != 2 || counts[session.mainURI] != 1 || counts[helperURI] != 1 {
		test.Errorf("edits by file = %v, want one in main.gol and one in helper.gol", counts)
	}
}

// checkImplOutline checks that the outline range of an impl method starts
// at its func keyword. gosling adds the receiver after that keyword.
func (session *lspSession) checkImplOutline(test *testing.T) {
	helperURI := fileURI(filepath.Join(session.dir, "helper.gol"))
	raw := session.client.request("textDocument/documentSymbol", map[string]any{"textDocument": map[string]any{"uri": helperURI}})
	var symbols []struct {
		Name     string      `json:"name"`
		Location lspLocation `json:"location"`
	}
	if err := json.Unmarshal(raw, &symbols); err != nil {
		test.Fatalf("decode %s: %v", raw, err)
	}
	want := positionOf(test, lspHelper, "func bump")
	for _, symbol := range symbols {
		if strings.Contains(symbol.Name, "bump") {
			if symbol.Location.Range.Start != want {
				test.Errorf("%s starts at %+v, want %+v", symbol.Name, symbol.Location.Range.Start, want)
			}
			return
		}
	}
	test.Errorf("outline %s has no bump method", raw)
}

func (session *lspSession) checkGenerateOnSave(test *testing.T) {
	session.save(test)
	for _, name := range []string{"main.go", "helper.go"} {
		generated, err := os.ReadFile(filepath.Join(session.dir, name))
		if err != nil {
			test.Fatalf("save did not write %s: %v", name, err)
		}
		if !strings.HasPrefix(string(generated), transpile.GeneratedPrefix) {
			test.Errorf("%s does not start with the generated header", name)
		}
	}
}

func (session *lspSession) checkSaveWithErrors(test *testing.T) {
	mainGoPath := filepath.Join(session.dir, "main.go")
	if err := os.Remove(mainGoPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		test.Fatal(err)
	}
	session.changeMain(strings.Replace(lspMain, "return string(data), nil", "return string(data), nil +", 1))
	session.save(test)
	if _, err := os.Stat(mainGoPath); !errors.Is(err, fs.ErrNotExist) {
		test.Errorf("save with a syntax error wrote main.go: %v", err)
	}
	session.changeMain(lspMain)
}

func (session *lspSession) checkFileOperationCapability(test *testing.T) {
	var initialize struct {
		Capabilities struct {
			Workspace struct {
				FileOperations map[string]struct {
					Filters []struct {
						Pattern struct {
							Glob string `json:"glob"`
						} `json:"pattern"`
					} `json:"filters"`
				} `json:"fileOperations"`
			} `json:"workspace"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(session.client.initializeResult, &initialize); err != nil {
		test.Fatal(err)
	}
	for _, operation := range []string{"didRename", "didDelete"} {
		registration := initialize.Capabilities.Workspace.FileOperations[operation]
		if len(registration.Filters) != 1 || registration.Filters[0].Pattern.Glob != "**/*.gol" {
			test.Errorf("%s registration = %+v, want one **/*.gol filter", operation, registration)
		}
	}
}

func (session *lspSession) notifyFileOperation(test *testing.T, method string, files []map[string]string) {
	test.Helper()
	session.client.notify(method, map[string]any{"files": files})
	session.waitUntilHandled(test)
}

func (session *lspSession) checkFileRename(test *testing.T) {
	writeTree(test, session.dir, map[string]string{"old.gol": "package main\n\nfunc oldName() int { return 1 }\n"})
	session.save(test)
	oldGoPath := filepath.Join(session.dir, "old.go")
	if _, err := os.Stat(oldGoPath); err != nil {
		test.Fatalf("save did not write old.go: %v", err)
	}
	oldGolPath, newGolPath := filepath.Join(session.dir, "old.gol"), filepath.Join(session.dir, "new.gol")
	newGoPath := filepath.Join(session.dir, "new.go")
	if err := os.Rename(oldGolPath, newGolPath); err != nil {
		test.Fatal(err)
	}
	session.notifyFileOperation(test, "workspace/didRenameFiles", []map[string]string{{"oldUri": fileURI(oldGolPath), "newUri": fileURI(newGolPath)}})
	if _, err := os.Stat(oldGoPath); !errors.Is(err, fs.ErrNotExist) {
		test.Errorf("old.go still exists after the rename: %v", err)
	}
	if _, err := os.Stat(newGoPath); err != nil {
		test.Errorf("rename did not write new.go: %v", err)
	}
}

func (session *lspSession) checkFileDelete(test *testing.T) {
	newGolPath, newGoPath := filepath.Join(session.dir, "new.gol"), filepath.Join(session.dir, "new.go")
	if _, err := os.Stat(newGoPath); err != nil {
		test.Fatalf("new.go must exist before the delete: %v", err)
	}
	if err := os.Remove(newGolPath); err != nil {
		test.Fatal(err)
	}
	session.notifyFileOperation(test, "workspace/didDeleteFiles", []map[string]string{{"uri": fileURI(newGolPath)}})
	if _, err := os.Stat(newGoPath); !errors.Is(err, fs.ErrNotExist) {
		test.Errorf("new.go still exists after the delete: %v", err)
	}
}

// checkFormatting removes the indentation of main.gol. The edits from the
// server must restore it.
func (session *lspSession) checkFormatting(test *testing.T) {
	var capabilities struct {
		Capabilities struct {
			DocumentFormattingProvider bool `json:"documentFormattingProvider"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(session.client.initializeResult, &capabilities); err != nil {
		test.Fatal(err)
	}
	if !capabilities.Capabilities.DocumentFormattingProvider {
		test.Error("the server does not list documentFormattingProvider")
	}
	lines := strings.Split(lspMain, "\n")
	for lineIndex := range lines {
		lines[lineIndex] = strings.TrimLeft(lines[lineIndex], "\t")
	}
	unindented := strings.Join(lines, "\n")
	session.changeMain(unindented)
	raw := session.client.request("textDocument/formatting", map[string]any{
		"textDocument": map[string]any{"uri": session.mainURI},
		"options":      map[string]any{"tabSize": 4, "insertSpaces": false},
	})
	var edits []struct {
		Range struct {
			Start lspPosition `json:"start"`
			End   lspPosition `json:"end"`
		} `json:"range"`
		NewText string `json:"newText"`
	}
	if err := json.Unmarshal(raw, &edits); err != nil {
		test.Fatalf("decode %s: %v", raw, err)
	}
	formatted := unindented
	for editIndex := len(edits) - 1; editIndex >= 0; editIndex-- {
		edit := edits[editIndex]
		start, end := offsetOf(formatted, edit.Range.Start), offsetOf(formatted, edit.Range.End)
		formatted = formatted[:start] + edit.NewText + formatted[end:]
	}
	if formatted != lspMain {
		test.Errorf("formatted text =\n%s\nwant\n%s", formatted, lspMain)
	}
	session.changeMain(strings.Replace(unindented, "return string(data), nil", "return string(data), nil +", 1))
	raw = session.client.request("textDocument/formatting", map[string]any{"textDocument": map[string]any{"uri": session.mainURI}})
	if string(raw) != "[]" {
		test.Errorf("formatting with a syntax error = %s, want no edits", raw)
	}
	session.changeMain(lspMain)
}

// offsetOf returns the offset of an ASCII position in text.
func offsetOf(text string, cursor lspPosition) int {
	offset := 0
	for range cursor.Line {
		offset += strings.IndexByte(text[offset:], '\n') + 1
	}
	return offset + cursor.Character
}
