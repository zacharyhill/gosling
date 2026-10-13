package transpile

import (
	"go/importer"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func transpileWithMap(test *testing.T, path string, src []byte, lineDirectives bool) Output {
	test.Helper()
	outputs, err := Transpile(Config{Importer: importer.Default(), LineDirectives: lineDirectives}, []Input{{Path: path, Src: src}})
	if err != nil {
		test.Fatalf("Transpile: %v", err)
	}
	return outputs[0]
}

func TestSourceMapSpansMatchText(test *testing.T) {
	paths, err := filepath.Glob("testdata/golden/*.gol")
	if err != nil {
		test.Fatal(err)
	}
	for _, path := range paths {
		src, err := os.ReadFile(path)
		if err != nil {
			test.Fatal(err)
		}
		for _, lineDirectives := range []bool{true, false} {
			checkSpansMatchText(test, path, src, lineDirectives)
		}
	}
}

func checkSpansMatchText(test *testing.T, path string, src []byte, lineDirectives bool) {
	test.Helper()
	output := transpileWithMap(test, path, src, lineDirectives)
	if len(output.SourceMap.byGo) == 0 {
		test.Errorf("%s: source map is empty", path)
	}
	for _, span := range output.SourceMap.byGo {
		golText := string(src[span.golOffset : span.golOffset+span.length])
		goText := string(output.Src[span.goOffset : span.goOffset+span.length])
		if golText != goText {
			test.Errorf("%s (lines %v): span maps %q to %q", path, lineDirectives, golText, goText)
		}
	}
}

// minIdentCoverage is the share of identifiers in each golden file that must
// map to the output. gosling regenerates some names, such as catch bindings
// and destructuring patterns, so the share is below 1.
const minIdentCoverage = 0.75

func TestSourceMapCoversIdentifiers(test *testing.T) {
	keywords := map[string]bool{"let": true, "try": true, "catch": true, "finally": true, "of": true, "t": true}
	paths, err := filepath.Glob("testdata/golden/*.gol")
	if err != nil {
		test.Fatal(err)
	}
	for _, path := range paths {
		src, err := os.ReadFile(path)
		if err != nil {
			test.Fatal(err)
		}
		output := transpileWithMap(test, path, src, true)
		mapped, total := 0, 0
		for _, sourceToken := range scanTokens(src) {
			if !token.IsIdentifier(sourceToken.text) || keywords[sourceToken.text] {
				continue
			}
			total++
			if _, ok := output.SourceMap.GoOffset(sourceToken.offset); ok {
				mapped++
			}
		}
		if float64(mapped) < minIdentCoverage*float64(total) {
			test.Errorf("%s: %d of %d identifiers map to the output, want at least %.0f%%", path, mapped, total, minIdentCoverage*100)
		}
	}
}

func TestSourceMapLookups(test *testing.T) {
	src := "package p\n\nimport \"strconv\"\n\nfunc parse(text string) (int, error) {\n\ttry {\n\t\tconst count = strconv.Atoi(text) * 2\n\t\treturn count, nil\n\t} catch (e) {\n\t\treturn 0, e\n\t}\n}\n"
	for _, lineDirectives := range []bool{true, false} {
		output := transpileWithMap(test, "x.gol", []byte(src), lineDirectives)
		generated := string(output.Src)
		for _, name := range []string{"Atoi", "strconv", "count, nil", "parse", "text)"} {
			golOffset := strings.Index(src, name)
			goOffset, ok := output.SourceMap.GoOffset(golOffset)
			if !ok {
				test.Errorf("GoOffset(%q) not found\n%s", name, generated)
				continue
			}
			if !strings.HasPrefix(generated[goOffset:], name) {
				test.Errorf("GoOffset(%q) points at %q", name, generated[goOffset:min(goOffset+20, len(generated))])
			}
			backOffset, ok := output.SourceMap.GolOffset(goOffset)
			if !ok || backOffset != golOffset {
				test.Errorf("GolOffset(GoOffset(%q)) = %d, %v, want %d", name, backOffset, ok, golOffset)
			}
		}
		endOfName := strings.Index(src, "Atoi") + len("Atoi")
		goOffset, ok := output.SourceMap.GoOffset(endOfName)
		if !ok || !strings.HasPrefix(generated[goOffset:], "(") {
			test.Errorf("GoOffset at the end of Atoi = %d, %v, want the offset before (", goOffset, ok)
		}
	}
}

func TestSourceMapGeneratedTextHasNoSource(test *testing.T) {
	src := "package p\n\nimport \"os\"\n\nfunc remove(path string) {\n\ttry {\n\t\tos.Remove(path)\n\t} catch (e) {\n\t\tprintln(e)\n\t}\n}\n"
	output := transpileWithMap(test, "x.gol", []byte(src), false)
	gotoOffset := strings.Index(string(output.Src), "goto")
	if gotoOffset < 0 {
		test.Fatalf("no goto in output\n%s", output.Src)
	}
	if golOffset, ok := output.SourceMap.GolOffset(gotoOffset); ok {
		test.Errorf("generated goto maps to %d, want no source", golOffset)
	}
	nearOffset, ok := output.SourceMap.GolOffsetNear(gotoOffset)
	wantNear := strings.Index(src, "os.Remove(path)") + len("os.Remove(path")
	if !ok || nearOffset != wantNear {
		test.Errorf("GolOffsetNear(goto) = %d, %v, want %d, the end of the token before it", nearOffset, ok, wantNear)
	}
}
