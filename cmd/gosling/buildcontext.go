package main

import (
	"bufio"
	"bytes"
	"fmt"
	"go/build"
	"go/build/constraint"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// knownOS and knownArch are the GOOS and GOARCH values that file names and
// build constraints can use. They copy the lists in go/build, which does
// not export them.
var (
	knownOS = map[string]bool{
		"aix": true, "android": true, "darwin": true, "dragonfly": true, "freebsd": true, "hurd": true,
		"illumos": true, "ios": true, "js": true, "linux": true, "nacl": true, "netbsd": true,
		"openbsd": true, "plan9": true, "solaris": true, "wasip1": true, "windows": true, "zos": true,
	}
	knownArch = map[string]bool{
		"386": true, "amd64": true, "amd64p32": true, "arm": true, "armbe": true, "arm64": true,
		"arm64be": true, "loong64": true, "mips": true, "mipsle": true, "mips64": true, "mips64le": true,
		"mips64p32": true, "mips64p32le": true, "ppc": true, "ppc64": true, "ppc64le": true, "riscv": true,
		"riscv64": true, "s390": true, "s390x": true, "sparc": true, "sparc64": true, "wasm": true,
	}
)

// usualArch gives the GOARCH for a GOOS that does not run on the usual
// GOARCH values.
var usualArch = map[string]string{"js": "wasm", "wasip1": "wasm", "aix": "ppc64", "zos": "s390x"}

// otherOS and otherArch are the values that replace the host GOOS or
// GOARCH when a constraint excludes it, as in "!linux".
var (
	otherOS   = []string{"linux", "windows", "darwin", "freebsd"}
	otherArch = []string{"amd64", "arm64", "386"}
)

// maxConstraintTags limits the tags that chooseContext combines. The number
// of combinations is 2 to the power of the tag count.
const maxConstraintTags = 10

func isDefaultContext(context build.Context) bool {
	return formatContextKey(context) == formatContextKey(build.Default)
}

func formatContextKey(context build.Context) string {
	return fmt.Sprintf("%s/%s cgo=%t tags=%s", context.GOOS, context.GOARCH, context.CgoEnabled, strings.Join(context.BuildTags, ","))
}

// matchesGol reports whether a .gol file matches context, as a .go file
// with the same name and text would. MatchFile reads only .go files, and the
// part of a .gol file that it reads is plain Go.
func matchesGol(context build.Context, path string, src []byte) (bool, error) {
	context.OpenFile = func(string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(src)), nil
	}
	dir, name := filepath.Split(path)
	return context.MatchFile(dir, strings.TrimSuffix(name, sourceExt)+".go")
}

// chooseContext returns the build configuration that type-checks a .gol file:
// build.Default if the file matches it. Otherwise it tries the tags in the
// build constraints and the file name, fewest first, until the file matches.
func chooseContext(path string, src []byte) (build.Context, error) {
	if matches, err := matchesGol(build.Default, path, src); err != nil || matches {
		return build.Default, err
	}
	tags := parseConstraintTags(path, src)
	if len(tags) > maxConstraintTags {
		tags = tags[:maxConstraintTags]
	}
	for subsetSize := 0; subsetSize <= len(tags); subsetSize++ {
		for _, chosen := range buildCombinations(tags, subsetSize) {
			context, ok := buildContextWithTags(tags, chosen)
			if !ok {
				continue
			}
			matches, err := matchesGol(context, path, src)
			if err != nil {
				return build.Default, err
			}
			if matches {
				return context, nil
			}
		}
	}
	return build.Default, fmt.Errorf("%s: no build configuration matches its build constraints", path)
}

// buildContextWithTags returns build.Default with the chosen tags true and the
// other mentioned tags false. It reports false for a contradiction, such as
// two operating systems.
func buildContextWithTags(mentioned, chosen []string) (build.Context, bool) {
	context := build.Default
	context.BuildTags = nil
	var systems, arches []string
	for _, tag := range chosen {
		switch {
		case knownOS[tag]:
			systems = append(systems, tag)
		case knownArch[tag]:
			arches = append(arches, tag)
		case tag != "cgo" && tag != "unix":
			context.BuildTags = append(context.BuildTags, tag)
		}
	}
	if len(systems) > 1 || len(arches) > 1 {
		return context, false
	}
	isMentioned := func(tag string) bool { return slices.Contains(mentioned, tag) }
	isFree := func(tag string) bool { return !isMentioned(tag) }
	switch {
	case len(systems) == 1:
		context.GOOS = systems[0]
	case isMentioned(context.GOOS):
		context.GOOS = otherOS[max(slices.IndexFunc(otherOS, isFree), 0)]
	}
	switch {
	case len(arches) == 1:
		context.GOARCH = arches[0]
	case usualArch[context.GOOS] != "":
		context.GOARCH = usualArch[context.GOOS]
	case isMentioned(context.GOARCH):
		context.GOARCH = otherArch[max(slices.IndexFunc(otherArch, isFree), 0)]
	}
	switch {
	case isMentioned("cgo"):
		context.CgoEnabled = slices.Contains(chosen, "cgo")
	case context.GOOS != build.Default.GOOS || context.GOARCH != build.Default.GOARCH:
		// As in the go tool, cgo is off for another system by default.
		context.CgoEnabled = false
	}
	return context, true
}

// parseConstraintTags returns the tags in the build constraints of a file and
// the GOOS and GOARCH parts of its name, without Go release tags.
func parseConstraintTags(path string, src []byte) []string {
	var tags []string
	addTag := func(tag string) {
		if !strings.HasPrefix(tag, "go1.") && !slices.Contains(tags, tag) {
			tags = append(tags, tag)
		}
	}
	lineScanner := bufio.NewScanner(bytes.NewReader(src))
	for lineScanner.Scan() {
		line := strings.TrimSpace(lineScanner.Text())
		if line != "" && !strings.HasPrefix(line, "//") {
			break
		}
		if !constraint.IsGoBuild(line) && !constraint.IsPlusBuild(line) {
			continue
		}
		if expression, err := constraint.Parse(line); err == nil {
			walkTags(expression, addTag)
		}
	}
	name := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(path), sourceExt), "_test")
	parts := strings.Split(name, "_")
	for _, part := range parts[1:] {
		if knownOS[part] || knownArch[part] {
			addTag(part)
		}
	}
	return tags
}

func walkTags(expression constraint.Expr, addTag func(string)) {
	switch typedExpr := expression.(type) {
	case *constraint.TagExpr:
		addTag(typedExpr.Tag)
	case *constraint.NotExpr:
		walkTags(typedExpr.X, addTag)
	case *constraint.AndExpr:
		walkTags(typedExpr.X, addTag)
		walkTags(typedExpr.Y, addTag)
	case *constraint.OrExpr:
		walkTags(typedExpr.X, addTag)
		walkTags(typedExpr.Y, addTag)
	}
}

// buildCombinations returns each subset of items with subsetSize elements,
// in order.
func buildCombinations(items []string, subsetSize int) [][]string {
	if subsetSize == 0 {
		return [][]string{nil}
	}
	var subsets [][]string
	for firstIndex := 0; firstIndex+subsetSize <= len(items); firstIndex++ {
		for _, rest := range buildCombinations(items[firstIndex+1:], subsetSize-1) {
			subsets = append(subsets, append([]string{items[firstIndex]}, rest...))
		}
	}
	return subsets
}

// buildContextEnv returns the environment and build flags that make the go
// tool load packages for context. The ignore tag marks files that no build
// uses, also in the standard library, where it causes import cycles. So the
// imports load without it.
func buildContextEnv(context build.Context) ([]string, []string) {
	if isDefaultContext(context) {
		return nil, nil
	}
	cgoValue := "0"
	if context.CgoEnabled {
		cgoValue = "1"
	}
	env := append(os.Environ(), "GOOS="+context.GOOS, "GOARCH="+context.GOARCH, "CGO_ENABLED="+cgoValue)
	tags := slices.DeleteFunc(slices.Clone(context.BuildTags), func(tag string) bool { return tag == "ignore" })
	if len(tags) == 0 {
		return env, nil
	}
	return env, []string{"-tags=" + strings.Join(tags, ",")}
}
