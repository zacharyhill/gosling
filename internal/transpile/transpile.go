// Package transpile converts gosling source to plain Go.
package transpile

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/ast/astutil"

	"github.com/zacharyhill/gosling/internal/preprocess"
)

// maxCheckPasses limits the type-check loop. Each pass can find more
// fallible assignments after earlier ones stop giving invalid types.
const maxCheckPasses = 20

// checkPath is the import path for type checking. It cannot clash with a
// real import path.
const checkPath = "gosling.invalid/checked"

// Input is one .gol source file.
type Input struct {
	Path string
	Src  []byte
}

// Output is the generated Go file for one Input.
type Output struct {
	Path      string
	Src       []byte
	SourceMap SourceMap
}

// Config holds the options for Transpile.
type Config struct {
	Importer types.Importer
	// GoFiles are the plain .go files of the package. Type checking needs them.
	GoFiles []string
	// LineDirectives emits /*line*/ comments so compiler errors point at the .gol file.
	LineDirectives bool
	// KeepGoing emits the files that lower without errors, even when type
	// checking fails. Transpile then returns the outputs and the error.
	// Editors use this, because code being edited often has type errors.
	KeepGoing bool
}

// OutputPath returns the .go path generated for a .gol path.
func OutputPath(path string) string {
	return strings.TrimSuffix(path, filepath.Ext(path)) + ".go"
}

// IsGenerated reports whether src starts with the header that gosling writes.
func IsGenerated(src []byte) bool {
	return bytes.HasPrefix(src, []byte(GeneratedPrefix))
}

// IsGeneratedFile reports whether the file at path is generated. It reads
// only the header.
func IsGeneratedFile(path string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	header := make([]byte, len(GeneratedPrefix))
	if _, err := io.ReadFull(file, header); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return false, nil
		}
		return false, err
	}
	return IsGenerated(header), nil
}

type file struct {
	path         string
	original     []byte
	preprocessed *preprocess.Result
	ast          *ast.File
	tokenFile    *token.File
	tries        map[int]preprocess.Try // keyed by offset of the "if"
	// augmented maps each assignment with an added blank identifier to its
	// original LHS length.
	augmented map[*ast.AssignStmt]int
	// optional holds selectors written with ?. instead of a plain dot.
	optional map[*ast.SelectorExpr]bool
	// coalesce maps each ?? marker call to the expression it replaced.
	coalesce   map[*ast.CallExpr]*ast.BinaryExpr
	arrows     []*arrow
	arrayCalls map[*ast.CallExpr]*arrayCall
	// identNames holds every identifier name in the source. Added imports
	// must not use these names.
	identNames map[string]bool
	// added maps the path of each import gosling adds to its name.
	added map[string]string
	// aliased holds the paths in added whose name is an alias.
	aliased map[string]bool
	// finallyOf maps a try statement to its finally block. prepareFinally
	// moves the block in front of the try.
	finallyOf     map[*ast.IfStmt]*ast.BlockStmt
	finallyBlocks map[*ast.BlockStmt]bool
	// hoisted maps each hoist marker call to the call it wraps.
	hoisted map[*ast.CallExpr]*ast.CallExpr
	// shorthand maps each keyed element from struct shorthand to its
	// source identifier.
	shorthand map[*ast.KeyValueExpr]*ast.Ident
	// badSpreads holds struct spread updates with unnamed fields. Rewriting
	// already reports them, so isSpurious drops their type errors.
	badSpreads map[*ast.CompositeLit]bool
	// panicAtEnd holds function bodies that end in a try that exits only
	// through calls that never return. Go does not count these calls as
	// terminating, so the output ends with panic("unreachable").
	panicAtEnd map[*ast.BlockStmt]bool
	// constVars holds the variables that a const declares. Code cannot
	// assign to them.
	constVars map[*ast.Ident]bool
	// constDecls maps each const that became a variable to the := statement
	// that replaced it. A const that became a var declaration maps to nil.
	constDecls map[*ast.GenDecl]*ast.AssignStmt
	// forOfBlanks maps each for-of range statement to the "_" key that
	// preprocessing added.
	forOfBlanks map[*ast.RangeStmt]*ast.Ident
}

// position maps a position in the preprocessed file to the original source.
func (file *file) position(position token.Pos) token.Position {
	return file.positionAt(file.preprocessed.OrigOffset(file.tokenFile.Offset(position)))
}

func (file *file) positionAt(offset int) token.Position {
	line := 1 + bytes.Count(file.original[:offset], []byte("\n"))
	column := offset - (bytes.LastIndexByte(file.original[:offset], '\n') + 1) + 1
	return token.Position{Filename: file.path, Offset: offset, Line: line, Column: column}
}

func (file *file) text(node ast.Node) string {
	return string(file.preprocessed.Src[file.tokenFile.Offset(node.Pos()):file.tokenFile.Offset(node.End())])
}

type transpiler struct {
	config         Config
	fileSet        *token.FileSet
	files          []*file
	plainGoFiles   []*ast.File
	diagnostics    scanner.ErrorList
	checkedPackage *types.Package
	// unions maps each union type to its variants, in declaration order.
	unions map[*types.TypeName][]types.Type
}

// Transpile converts the .gol files of one package to Go.
func Transpile(config Config, inputs []Input) ([]Output, error) {
	session := &transpiler{config: config, fileSet: token.NewFileSet()}
	if err := session.parse(inputs); err != nil {
		return nil, err
	}
	typeInfo := session.check()
	if err := session.diagnostics.Err(); err != nil && !config.KeepGoing {
		return nil, err
	}
	outputs := make([]Output, 0, len(session.files))
	for _, file := range session.files {
		output, ok, err := session.lowerAndEmit(file, typeInfo)
		if err != nil {
			return nil, err
		}
		if ok {
			outputs = append(outputs, output)
		}
	}
	session.diagnostics.Sort()
	if err := session.diagnostics.Err(); err != nil {
		if config.KeepGoing {
			return outputs, err
		}
		return nil, err
	}
	return outputs, nil
}

// lowerAndEmit converts one file. It reports false if lowering found errors
// in the file. With KeepGoing, an emit failure or a panic also skips the
// file. Type errors can leave nodes that lowering does not expect.
func (transpiler *transpiler) lowerAndEmit(file *file, typeInfo *types.Info) (output Output, ok bool, err error) {
	if transpiler.config.KeepGoing {
		defer func() {
			if recover() != nil {
				output, ok, err = Output{}, false, nil
			}
		}()
	}
	diagnosticsBefore := len(transpiler.diagnostics)
	lowerer := newLowerer(transpiler, file, typeInfo)
	lowerer.lowerFile()
	if len(transpiler.diagnostics) > diagnosticsBefore {
		return Output{}, false, nil
	}
	src, sourceMap, err := lowerer.emit()
	if err != nil {
		if transpiler.config.KeepGoing {
			return Output{}, false, nil
		}
		return Output{}, false, err
	}
	return Output{Path: OutputPath(file.path), Src: src, SourceMap: sourceMap}, true, nil
}

func (transpiler *transpiler) parse(inputs []Input) error {
	for _, input := range inputs {
		if err := transpiler.parseInput(input); err != nil {
			return err
		}
	}
	for _, path := range transpiler.config.GoFiles {
		plainFile, err := parser.ParseFile(transpiler.fileSet, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		transpiler.plainGoFiles = append(transpiler.plainGoFiles, plainFile)
	}
	if err := transpiler.avoidPackageNamesInTemps(); err != nil {
		return err
	}
	transpiler.diagnostics.Sort()
	return transpiler.diagnostics.Err()
}

// parseInput preprocesses and parses one .gol file. It records syntax
// errors as diagnostics and returns only unexpected failures.
func (transpiler *transpiler) parseInput(input Input) error {
	transpiler.reportGoDeclarations(input)
	golFile, err := transpiler.buildFile(input, nil)
	if err != nil || golFile == nil {
		return err
	}
	transpiler.files = append(transpiler.files, golFile)
	return nil
}

// buildFile preprocesses and parses one .gol file. New temporary variables
// do not use reservedNames. It returns nil after a syntax error.
func (transpiler *transpiler) buildFile(input Input, reservedNames map[string]bool) (*file, error) {
	preprocessed, err := preprocess.ProcessReserving(input.Src, reservedNames)
	if err != nil {
		var preprocessError *preprocess.Error
		if !errors.As(err, &preprocessError) {
			return nil, err
		}
		failedFile := &file{path: input.Path, original: input.Src}
		transpiler.diagnostics.Add(failedFile.positionAt(preprocessError.Offset), preprocessError.Msg)
		return nil, nil
	}
	golFile := newFile(input, preprocessed)
	astFile, err := parser.ParseFile(transpiler.fileSet, input.Path, preprocessed.Src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		transpiler.addParseErrors(golFile, err)
		return nil, nil
	}
	golFile.ast = astFile
	golFile.tokenFile = transpiler.fileSet.File(astFile.Pos())
	golFile.recordIdentNames()
	golFile.recordConstNames()
	golFile.recordForOfBlanks()
	for _, preprocessedTry := range preprocessed.Tries {
		golFile.tries[preprocessedTry.If] = preprocessedTry
	}
	golFile.prepareFinally()
	golFile.prepareFilters()
	golFile.stripMarkers()
	golFile.findArrows()
	return golFile, nil
}

// reportGoDeclarations reports each := and each var in a function in a .gol
// file. let and const replace them.
func (transpiler *transpiler) reportGoDeclarations(input Input) {
	golFile := &file{path: input.Path, original: input.Src}
	// The Go parser reports scanner errors later.
	sourceScanner, tokenFile := newScanner(input.Src, 0)
	braceDepth := 0
	for {
		position, kind, _ := sourceScanner.Scan()
		switch kind {
		case token.EOF:
			return
		case token.LBRACE:
			braceDepth++
		case token.RBRACE:
			braceDepth--
		case token.DEFINE:
			transpiler.diagnostics.Add(golFile.positionAt(tokenFile.Offset(position)), "use let or const instead of :=")
		case token.VAR:
			// Package-level var stays allowed, because let has no grouped
			// form.
			if braceDepth > 0 {
				transpiler.diagnostics.Add(golFile.positionAt(tokenFile.Offset(position)), "use let instead of var in a function")
			}
		}
	}
}

func newFile(input Input, preprocessed *preprocess.Result) *file {
	return &file{
		path:          input.Path,
		original:      input.Src,
		preprocessed:  preprocessed,
		tries:         map[int]preprocess.Try{},
		augmented:     map[*ast.AssignStmt]int{},
		optional:      map[*ast.SelectorExpr]bool{},
		coalesce:      map[*ast.CallExpr]*ast.BinaryExpr{},
		arrayCalls:    map[*ast.CallExpr]*arrayCall{},
		identNames:    map[string]bool{},
		added:         map[string]string{},
		aliased:       map[string]bool{},
		finallyOf:     map[*ast.IfStmt]*ast.BlockStmt{},
		finallyBlocks: map[*ast.BlockStmt]bool{},
		hoisted:       map[*ast.CallExpr]*ast.CallExpr{},
		shorthand:     map[*ast.KeyValueExpr]*ast.Ident{},
		badSpreads:    map[*ast.CompositeLit]bool{},
		panicAtEnd:    map[*ast.BlockStmt]bool{},
		constVars:     map[*ast.Ident]bool{},
		constDecls:    map[*ast.GenDecl]*ast.AssignStmt{},
		forOfBlanks:   map[*ast.RangeStmt]*ast.Ident{},
	}
}

func (file *file) recordIdentNames() {
	ast.Inspect(file.ast, func(node ast.Node) bool {
		if ident, ok := node.(*ast.Ident); ok {
			file.identNames[ident.Name] = true
		}
		return true
	})
}

func (transpiler *transpiler) addParseErrors(file *file, err error) {
	var errorList scanner.ErrorList
	if !errors.As(err, &errorList) {
		transpiler.diagnostics.Add(token.Position{Filename: file.path}, err.Error())
		return
	}
	for _, parseError := range errorList {
		transpiler.diagnostics.Add(file.positionAt(file.preprocessed.OrigOffset(parseError.Pos.Offset)), parseError.Msg)
	}
}

// stripMarkers changes optional selectors and ?? operators back into plain
// Go so the AST type-checks. It records the changed nodes.
func (file *file) stripMarkers() {
	coalesceAt := map[int]bool{}
	for _, offset := range file.preprocessed.Coalesce {
		coalesceAt[offset] = true
	}
	astutil.Apply(file.ast, nil, func(cursor *astutil.Cursor) bool {
		switch node := cursor.Node().(type) {
		case *ast.SelectorExpr:
			if node.Sel.Name == preprocess.OMarker {
				file.stripOptionalMarker(cursor, node)
			}
		case *ast.BinaryExpr:
			if node.Op == token.LOR && coalesceAt[file.tokenFile.Offset(node.OpPos)] {
				file.stripCoalesceMarker(cursor, node)
			}
		}
		return true
	})
}

func (file *file) stripOptionalMarker(cursor *astutil.Cursor, marker *ast.SelectorExpr) {
	// The marker is always between X and the real selector.
	if parent, ok := cursor.Parent().(*ast.SelectorExpr); ok {
		file.optional[parent] = true
	}
	cursor.Replace(marker.X)
}

func (file *file) stripCoalesceMarker(cursor *astutil.Cursor, operation *ast.BinaryExpr) {
	call := &ast.CallExpr{
		Fun:    &ast.Ident{Name: coalesceMarker, NamePos: operation.X.Pos()},
		Args:   []ast.Expr{operation.X, operation.Y},
		Rparen: operation.Y.End() - 1,
	}
	file.coalesce[call] = operation
	cursor.Replace(call)
}

func (transpiler *transpiler) check() *types.Info {
	astFiles, err := transpiler.filesToCheck()
	if err != nil {
		transpiler.diagnostics.Add(token.Position{}, "internal error: "+err.Error())
		return nil
	}
	var typeInfo *types.Info
	var typeErrors []types.Error
	for range maxCheckPasses {
		typeInfo, typeErrors = transpiler.typeCheck(astFiles)
		if !transpiler.augment(typeInfo) {
			break
		}
	}
	transpiler.checkConstAssignments(astFiles, typeInfo)
	if transpiler.unresolvedArrows() {
		return typeInfo // lowering reports the arrows, other errors are noise
	}
	badUnions := transpiler.collectUnions()
	for _, typeError := range typeErrors {
		switch {
		case transpiler.isSpurious(typeError, typeInfo, badUnions):
		case isTryCallError(typeError):
			transpiler.diagnostics.Add(transpiler.position(transpiler.findTryCall(typeError.Pos)), errTryCall)
		default:
			transpiler.diagnostics.Add(transpiler.position(typeError.Pos), cleanMessage(typeError.Msg))
		}
	}
	return typeInfo
}

// filesToCheck returns the .gol files, the plain .go files of the package,
// and a file that declares the markers.
func (transpiler *transpiler) filesToCheck() ([]*ast.File, error) {
	astFiles := make([]*ast.File, 0, len(transpiler.files)+len(transpiler.plainGoFiles)+1)
	for _, file := range transpiler.files {
		astFiles = append(astFiles, file.ast)
	}
	astFiles = append(astFiles, transpiler.plainGoFiles...)
	markersFile, err := parser.ParseFile(transpiler.fileSet, "gosling_markers.go", markerDecls(astFiles[0].Name.Name), 0)
	if err != nil {
		return nil, err
	}
	return append(astFiles, markersFile), nil
}

// typeCheck runs one type-check pass and collects all its errors.
func (transpiler *transpiler) typeCheck(astFiles []*ast.File) (*types.Info, []types.Error) {
	typeInfo := &types.Info{
		Types:     map[ast.Expr]types.TypeAndValue{},
		Defs:      map[*ast.Ident]types.Object{},
		Uses:      map[*ast.Ident]types.Object{},
		Scopes:    map[ast.Node]*types.Scope{},
		Implicits: map[ast.Node]types.Object{},
	}
	var typeErrors []types.Error
	config := types.Config{
		Importer: transpiler.config.Importer,
		Error: func(err error) {
			var typeError types.Error
			if errors.As(err, &typeError) {
				typeErrors = append(typeErrors, typeError)
			}
		},
	}
	// Check also returns the first error. The Error handler already collects it.
	transpiler.checkedPackage, _ = config.Check(checkPath, transpiler.fileSet, astFiles, typeInfo)
	return typeInfo, typeErrors
}

// isSpurious reports whether gosling's rewriting caused typeError, or
// another diagnostic already covers it.
func (transpiler *transpiler) isSpurious(typeError types.Error, typeInfo *types.Info, badUnions map[*file]map[int]bool) bool {
	if transpiler.fmtUsedByTemplates(typeError) {
		return true
	}
	if file := transpiler.fileAt(typeError.Pos); file != nil {
		// A bad union variant also breaks its generated method. Rewriting
		// already reports bad spreads.
		if badUnions[file][file.preprocessed.OrigOffset(file.tokenFile.Offset(typeError.Pos))] || file.inBadSpread(typeError.Pos) {
			return true
		}
	}
	return typeError.Msg == "missing return" && transpiler.endsInNoReturnTry(typeError.Pos, typeInfo)
}

// endsInNoReturnTry reports whether the function body that closes at rbrace
// ends in a try that cannot complete. Calls such as t.Fatal and os.Exit
// count as never returning. It also marks the body in panicAtEnd.
func (transpiler *transpiler) endsInNoReturnTry(rbrace token.Pos, typeInfo *types.Info) bool {
	file := transpiler.fileAt(rbrace)
	if file == nil {
		return false
	}
	var body *ast.BlockStmt
	ast.Inspect(file.ast, func(node ast.Node) bool {
		var funcBody *ast.BlockStmt
		switch node := node.(type) {
		case *ast.FuncDecl:
			funcBody = node.Body
		case *ast.FuncLit:
			funcBody = node.Body
		}
		if funcBody != nil && funcBody.Rbrace == rbrace {
			body = funcBody
		}
		return body == nil
	})
	if body == nil {
		return false
	}
	tryStmt, ok := lastStmt(body.List).(*ast.IfStmt)
	if !ok || file.finallyOf[tryStmt] != nil {
		return false
	}
	if _, isTry := file.tries[file.tokenFile.Offset(tryStmt.Pos())]; !isTry || !terminates(tryStmt, neverReturns(typeInfo)) {
		return false
	}
	file.panicAtEnd[body] = true
	return true
}

// markerDecls declares the markers in preprocessed files, so these files
// type-check. gosling never emits this declaration file.
func markerDecls(packageName string) string {
	return "package " + packageName + "\n\n" +
		"type " + preprocess.UntypedMarker + " = any\n" +
		"type " + preprocess.ResultMarker + " = any\n" +
		"func " + preprocess.TMarker + "(...any) string { return \"\" }\n" +
		"func " + preprocess.ThrowMarker + "(error) any { return nil }\n" +
		"func " + preprocess.AsMarker + "(any) bool { return false }\n" +
		"func " + preprocess.IsMarker + "(error) bool { return false }\n" +
		"func " + preprocess.TryMarker + "[T any](v T, err error) func(T) T { return nil }\n" +
		"func " + coalesceMarker + "[T any](x any, d T) T { return d }\n" +
		"func " + coalescePtrMarker + "[T any](x *T, d T) T { return d }\n" +
		"func " + hoistMarker + "[T any](v T, err error) T { return v }\n" +
		spreadMarkerDecls +
		arrayMarkerDecls()
}

func (transpiler *transpiler) unresolvedArrows() bool {
	for _, file := range transpiler.files {
		for _, arrowFunc := range file.arrows {
			if arrowFunc.nilStandIn != nil || arrowFunc.hasNilResult {
				return true
			}
		}
	}
	return false
}

// fmtUsedByTemplates reports an unused fmt import in a file with template
// literals. Lowering makes these literals use fmt.
func (transpiler *transpiler) fmtUsedByTemplates(typeError types.Error) bool {
	if !strings.HasPrefix(typeError.Msg, `"fmt" imported`) || !strings.HasSuffix(typeError.Msg, "not used") {
		return false
	}
	file := transpiler.fileAt(typeError.Pos)
	return file != nil && bytes.Contains(file.preprocessed.Src, []byte(preprocess.TMarker+"("))
}

// augment rewrites the AST with type information from the last pass. It
// reports whether the AST changed. A change needs another pass.
func (transpiler *transpiler) augment(typeInfo *types.Info) bool {
	changed := false
	for _, file := range transpiler.files {
		changed = convertConsts(file, typeInfo) || changed
		changed = transpiler.rewriteArrayCalls(file, typeInfo) || changed
		changed = transpiler.wrapNested(file, typeInfo) || changed
		changed = transpiler.expandShorthand(file, typeInfo) || changed
		changed = transpiler.fixArrows(file, typeInfo) || changed
		changed = usePtrCoalesce(file, typeInfo) || changed
		changed = addBlankForErrors(file, typeInfo) || changed
		changed = dropForOfBlanks(file, typeInfo) || changed
	}
	return changed
}

func usePtrCoalesce(file *file, typeInfo *types.Info) bool {
	changed := false
	for call := range file.coalesce {
		callee := call.Fun.(*ast.Ident)
		_, isPointer := pointerElem(typeInfo.Types[call.Args[0]].Type)
		// The checker records a constant with its converted type, so check
		// for any constant, not only an untyped one.
		if callee.Name == coalesceMarker && isPointer && typeInfo.Types[call.Args[1]].Value != nil {
			callee.Name = coalescePtrMarker
			changed = true
		}
	}
	return changed
}

func (transpiler *transpiler) position(position token.Pos) token.Position {
	if file := transpiler.fileAt(position); file != nil {
		return file.position(position)
	}
	return transpiler.fileSet.Position(position)
}

func (transpiler *transpiler) fileAt(position token.Pos) *file {
	for _, file := range transpiler.files {
		if file.tokenFile.Base() <= int(position) && int(position) <= file.tokenFile.Base()+file.tokenFile.Size() {
			return file
		}
	}
	return nil
}

func (transpiler *transpiler) errorf(file *file, position token.Pos, format string, arguments ...any) {
	transpiler.diagnostics.Add(file.position(position), fmt.Sprintf(format, arguments...))
}
