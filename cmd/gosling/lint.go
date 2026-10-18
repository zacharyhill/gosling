package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/zacharyhill/gosling/internal/transpile"
)

// goPositionRE matches a position in a .go file, such as main.go:12:5 or
// main.go:12. A Windows path can start with a drive letter.
var goPositionRE = regexp.MustCompile(`((?:[A-Za-z]:)?[^\s:"'` + "`" + `]+\.go):(\d+)(?::(\d+))?`)

// runLint runs a linter and changes each position in a generated file in
// its output to the position in the .gol file. It returns the exit code of
// the linter.
func runLint(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: gosling lint command [arg...]")
		fmt.Fprintln(stderr, "example: gosling lint golangci-lint run ./...")
		return 2
	}
	var commandOut, commandErr bytes.Buffer
	command := exec.Command(args[0], args[1:]...)
	command.Stdin = os.Stdin
	command.Stdout = &commandOut
	command.Stderr = &commandErr
	runErr := command.Run()
	workDir, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "gosling lint: positions stay in .go files:", err)
	}
	mapper := &positionMapper{workDir: workDir, files: map[string]*mappedFile{}, problems: map[string]error{}, warnings: stderr}
	if _, err := stdout.Write(mapper.mapPositions(commandOut.Bytes())); err != nil {
		fmt.Fprintln(stderr, "gosling lint:", err)
		return 1
	}
	if _, err := stderr.Write(mapper.mapPositions(commandErr.Bytes())); err != nil {
		return 1
	}
	var exitError *exec.ExitError
	switch {
	case errors.As(runErr, &exitError):
		return exitError.ExitCode()
	case runErr != nil:
		fmt.Fprintln(stderr, "gosling lint:", runErr)
		return 1
	}
	return 0
}

// mappedFile is a generated file and the .gol file it came from.
type mappedFile struct {
	golPath   string
	golText   []byte
	goText    []byte
	sourceMap transpile.SourceMap
}

// positionMapper finds the source maps of generated files. It loads each
// directory one time.
type positionMapper struct {
	// workDir is empty if the working directory is unknown. Then no
	// position changes.
	workDir string
	// files maps the absolute path of a .go file to its source, or to nil
	// if gosling did not generate it or it cannot map its positions.
	files map[string]*mappedFile
	// problems holds why a generated file cannot map its positions. Each
	// problem shows one time, when the linter first names the file.
	problems map[string]error
	warnings io.Writer
}

var errOutOfDate = errors.New("out of date; run gosling")

func (mapper *positionMapper) mapPositions(output []byte) []byte {
	if mapper.workDir == "" {
		return output
	}
	return goPositionRE.ReplaceAllFunc(output, func(match []byte) []byte {
		submatches := goPositionRE.FindSubmatch(match)
		line, err := strconv.Atoi(string(submatches[2]))
		if err != nil {
			return match
		}
		column := 0
		if len(submatches[3]) > 0 {
			if column, err = strconv.Atoi(string(submatches[3])); err != nil {
				return match
			}
		}
		mapped, ok := mapper.mapPosition(string(submatches[1]), line, column)
		if !ok {
			return match
		}
		return []byte(mapped)
	})
}

// mapPosition maps a 1-based line and column in a .go file. A position in
// code that gosling generates maps to the nearest copied token before it.
// Without a column, the first token of the line maps.
func (mapper *positionMapper) mapPosition(goPath string, line, column int) (string, bool) {
	file := mapper.loadMappedFile(goPath)
	if file == nil {
		return "", false
	}
	goOffset, ok := findLineOffset(file.goText, line)
	if !ok {
		return "", false
	}
	if column > 0 {
		goOffset += column - 1
	} else {
		goOffset += len(file.goText[goOffset:]) - len(bytes.TrimLeft(file.goText[goOffset:], " \t"))
	}
	golOffset, ok := file.sourceMap.GolOffsetNear(min(goOffset, len(file.goText)))
	if !ok {
		return "", false
	}
	golLine := 1 + bytes.Count(file.golText[:golOffset], []byte("\n"))
	golColumn := golOffset - bytes.LastIndexByte(file.golText[:golOffset], '\n')
	return fmt.Sprintf("%s:%d:%d", file.golPath, golLine, golColumn), true
}

// findLineOffset returns the offset of a 1-based line.
func findLineOffset(text []byte, line int) (int, bool) {
	offset := 0
	for range line - 1 {
		newline := bytes.IndexByte(text[offset:], '\n')
		if newline < 0 {
			return 0, false
		}
		offset += newline + 1
	}
	return offset, true
}

// loadMappedFile returns the source of a generated .go file, or nil. The
// golPath of the result has the form of goPath, relative or absolute.
func (mapper *positionMapper) loadMappedFile(goPath string) *mappedFile {
	absolutePath := goPath
	if !filepath.IsAbs(goPath) {
		absolutePath = filepath.Join(mapper.workDir, goPath)
	}
	if _, loaded := mapper.files[absolutePath]; !loaded {
		mapper.loadDirOutputs(filepath.Dir(absolutePath), absolutePath)
	}
	if problem := mapper.problems[absolutePath]; problem != nil {
		delete(mapper.problems, absolutePath)
		fmt.Fprintf(mapper.warnings, "gosling lint: %s: %v\n", goPath, problem)
	}
	cached := mapper.files[absolutePath]
	if cached == nil {
		return nil
	}
	return &mappedFile{golPath: golPathOf(goPath), golText: cached.golText, goText: cached.goText, sourceMap: cached.sourceMap}
}

// loadDirOutputs generates the files of dir in memory. Each output must
// match the file on disk, or its positions are wrong.
func (mapper *positionMapper) loadDirOutputs(dir, requestedPath string) {
	mapper.files[requestedPath] = nil
	hasSource, err := hasGolSource(requestedPath)
	if err != nil {
		mapper.problems[requestedPath] = err
		return
	}
	if !hasSource {
		return
	}
	outputs, err := generator{}.Generate(dir, nil, true)
	if err != nil {
		fmt.Fprintf(mapper.warnings, "gosling lint: %s: %v\n", dir, err)
		return
	}
	for _, output := range outputs {
		// Generate joins the absolute dir with each file name, so the path
		// is absolute.
		mapper.files[output.Path] = nil
		file, err := loadOutput(output)
		if err != nil {
			mapper.problems[output.Path] = err
			continue
		}
		mapper.files[output.Path] = file
	}
}

// loadOutput reads the .go and .gol files of an output. It returns
// errOutOfDate if the .go file on disk differs from the output.
func loadOutput(output transpile.Output) (*mappedFile, error) {
	goTextOnDisk, err := os.ReadFile(output.Path)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(goTextOnDisk, output.Src) {
		return nil, errOutOfDate
	}
	golText, err := os.ReadFile(golPathOf(output.Path))
	if err != nil {
		return nil, err
	}
	return &mappedFile{golText: golText, goText: output.Src, sourceMap: output.SourceMap}, nil
}

func golPathOf(goPath string) string {
	return goPath[:len(goPath)-len(".go")] + sourceExt
}

// hasGolSource reports whether a .go file has a .gol file of the same name.
func hasGolSource(goPath string) (bool, error) {
	_, err := os.Stat(golPathOf(goPath))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}
