package main

import (
	"go/build"
	"slices"
	"testing"
)

func TestContextFor(test *testing.T) {
	host := build.Default
	for _, testCase := range []struct {
		name, constraint string
		check            func(build.Context) bool
	}{
		{"plain.gol", "", func(context build.Context) bool { return isDefaultContext(context) }},
		{"a_windows.gol", "", func(context build.Context) bool { return context.GOOS == "windows" }},
		{"b.gol", "//go:build !" + host.GOOS, func(context build.Context) bool { return context.GOOS != host.GOOS && !context.CgoEnabled }},
		{"c.gol", "//go:build integration", func(context build.Context) bool {
			return context.GOOS == host.GOOS && slices.Equal(context.BuildTags, []string{"integration"})
		}},
		{"d.gol", "//go:build js", func(context build.Context) bool { return context.GOOS == "js" && context.GOARCH == "wasm" }},
		{"e.gol", "// +build plan9,386", func(context build.Context) bool { return context.GOOS == "plan9" && context.GOARCH == "386" }},
	} {
		src := []byte(testCase.constraint + "\n\npackage p\n")
		context, err := chooseContext("/work/p/"+testCase.name, src)
		if err != nil {
			test.Errorf("%s: %v", testCase.name, err)
			continue
		}
		matches, err := matchesGol(context, "/work/p/"+testCase.name, src)
		if err != nil || !matches || !testCase.check(context) {
			test.Errorf("%s: context %s does not fit", testCase.name, formatContextKey(context))
		}
	}
	if _, err := chooseContext("/work/p/never.gol", []byte("//go:build linux && windows\n\npackage p\n")); err == nil {
		test.Error("want an error for a constraint that no system meets")
	}
}
