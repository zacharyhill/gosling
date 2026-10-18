package preprocess

import (
	"slices"
	"strings"
	"testing"
)

func TestProcess(test *testing.T) {
	testCases := []struct {
		name     string
		src      string
		want     string
		tries    []Try
		splits   []int
		coalesce []int
		unions   []Union
		consts   []int
		commaOK  []int
	}{
		{
			name: "try catch with binding",
			src:  "func f() {\n\ttry {\n\t\tg()\n\t} catch (e) {\n\t\th(e)\n\t}\n}",
			want: "func f() {\n\tif true {\n\t\tg()\n\t} else { var e error; _ = e;\n\t\th(e)\n\t}\n}",
			tries: []Try{{
				If:       len("func f() {\n\t"),
				HasCatch: true,
				CatchEnd: len("func f() {\n\tif true {\n\t\tg()\n\t} else { var e error; _ = e;"),
				Binding:  "e",
			}},
		},
		{
			name: "catch without binding",
			src:  "try { g() } catch { h() }",
			want: "if true { g() } else { h() }",
			tries: []Try{{
				If:       0,
				HasCatch: true,
				CatchEnd: len("if true { g() } else {"),
			}},
		},
		{
			name: "nested try",
			src:  "try { try { a() } catch (i) { b() } } catch (o) { c() }",
			want: "if true { if true { a() } else { var i error; _ = i; b() } } else { var o error; _ = o; c() }",
			tries: []Try{
				{If: 0, HasCatch: true, CatchEnd: len("if true { if true { a() } else { var i error; _ = i; b() } } else { var o error; _ = o;"), Binding: "o"},
				{If: len("if true { "), HasCatch: true, CatchEnd: len("if true { if true { a() } else { var i error; _ = i;"), Binding: "i"},
			},
		},
		{
			name:     "optional chain and nullish coalescing",
			src:      "n := user?.profile?.name ?? \"anon\"",
			want:     "n := user.__goslingO.profile.__goslingO.name || \"anon\"",
			coalesce: []int{len("n := user.__goslingO.profile.__goslingO.name ")},
		},
		{
			name: "optional method call",
			src:  "x := a?.Get(1)",
			want: "x := a.__goslingO.Get(1)",
		},
		{
			name: "arrow with untyped parameter",
			src:  "f(x => x * 2, y)",
			want: "f(func(x __goslingU) __goslingR { return x * 2 }, y)",
		},
		{
			name: "arrow with untyped parameter list",
			src:  "xs.reduce((acc, x) => acc + x, 0)",
			want: "xs.reduce(func(acc __goslingU, x __goslingU) __goslingR { return acc + x }, 0)",
		},
		{
			name: "arrow with typed parameters",
			src:  "double := (x int) => x * 2",
			want: "double := func(x int) __goslingR { return x * 2 }",
		},
		{
			name: "arrow with block body",
			src:  "run(() => {\n\tgo()\n})",
			want: "run(func() __goslingR {\n\tgo()\n})",
		},
		{
			name: "nested arrows end together",
			src:  "add := (a int) => (b int) => a + b\n",
			want: "add := func(a int) __goslingR { return func(b int) __goslingR { return a + b } }\n",
		},
		{
			name: "arrow body with composite literal and call",
			src:  "xs.map(x => P{x, f(x)}).filter(p => p.ok)",
			want: "xs.__goslingMap(func(x __goslingU) __goslingR { return P{x, f(x)} }).filter(func(p __goslingU) __goslingR { return p.ok })",
		},
		{
			name: "arrow inside interpolation",
			src:  "s := t`${xs.map(x => x + 1)}`",
			want: "s := __goslingT(``, xs.__goslingMap(func(x __goslingU) __goslingR { return x + 1 }), ``)",
		},
		{
			name: "try catch finally",
			src:  "try { a() } catch (e) { b() } finally { c() }",
			want: "if true { a() } else { var e error; _ = e; b() } ; { c() }",
			tries: []Try{{
				If:       0,
				HasCatch: true,
				CatchEnd: len("if true { a() } else { var e error; _ = e;"),
				Binding:  "e",
				Finally:  len("if true { a() } else { var e error; _ = e; b() } ; "),
			}},
		},
		{
			name:  "try finally",
			src:   "try { a() } finally { c() }",
			want:  "if true { a() } ; { c() }",
			tries: []Try{{If: 0, Finally: len("if true { a() } ; ")}},
		},
		{
			name: "slice spread",
			src:  "all := [...a, x, y, ...b]",
			want: "all := __goslingCat(a, __goslingOne(x, y), b)",
		},
		{
			name: "slice spread of one",
			src:  "c := [...a]",
			want: "c := __goslingCat(a)",
		},
		{
			name: "array type is not spread",
			src:  "a := [...]int{1, 2}",
			want: "a := [...]int{1, 2}",
		},
		{
			name: "struct spread",
			src:  "p2 := Point{...p, X: 5}",
			want: "p2 := Point(__goslingWith(p, Point{ X: 5}))",
		},
		{
			name: "struct spread alone",
			src:  "p2 := geo.Point{...p}",
			want: "p2 := geo.Point(__goslingWith(p, geo.Point{}))",
		},
		{
			name: "generic struct spread",
			src:  "b := Box[int]{...b0, V: f(x)}",
			want: "b := Box[int](__goslingWith(b0, Box[int]{ V: f(x)}))",
		},
		{
			name: "union type",
			src:  "type Shape = Circle | *Square\n",
			want: "type Shape interface{ isShape() }; func (Circle) isShape() {}; func (*Square) isShape() {}; func __goslingVariants_Shape(Circle, *Square) {}\n",
			splits: []int{
				len("type Shape interface{ isShape() }"),
				len("type Shape interface{ isShape() }; func (Circle) isShape() {}"),
			},
			unions: []Union{{
				Name:        "Shape",
				MarkerStart: len("type Shape interface{ isShape() }; func (Circle) isShape() {}; func (*Square) isShape() {}"),
				MarkerEnd:   len("type Shape interface{ isShape() }; func (Circle) isShape() {}; func (*Square) isShape() {}; func __goslingVariants_Shape(Circle, *Square) {}"),
			}},
		},
		{
			name: "plain alias untouched",
			src:  "type ID = string\n",
			want: "type ID = string\n",
		},
		{
			name: "template literal",
			src:  "s := t`hi ${name}, ${n + 1}!`",
			want: "s := __goslingT(`hi `, name, `, `, n + 1, `!`)",
		},
		{
			name:     "gosling syntax inside interpolation",
			src:      "s := t`${a?.b ?? c}`",
			want:     "s := __goslingT(``, a.__goslingO.b || c, ``)",
			coalesce: []int{len("s := __goslingT(``, a.__goslingO.b ")},
		},
		{
			name: "template literal without interpolation",
			src:  "s := t`plain`",
			want: "s := __goslingT(`plain`)",
		},
		{
			name: "template literal keeps newlines and escapes",
			src:  "s := t`a\\${x}\n${m[\"}\"]}\nb`",
			want: "s := __goslingT(`a\\${x}\n`, m[\"}\"], `\nb`)",
		},
		{
			name: "template literal with braces in expression",
			src:  "s := t`${Point{1, 2}}`",
			want: "s := __goslingT(``, Point{1, 2}, ``)",
		},
		{
			name: "object destructuring of simple expression",
			src:  "func f() {\n\t{a, b: c} := obj.inner\n}",
			want: "func f() {\n\ta, c := obj.inner.a, obj.inner.b\n}",
		},
		{
			name:   "object destructuring hoists call",
			src:    "func f() {\n\t{a, b} = load()\n}",
			want:   "func f() {\n\ttmp := load(); a, b = tmp.a, tmp.b\n}",
			splits: []int{len("func f() {\n\ttmp := load()")},
		},
		{
			name: "array destructuring with rest",
			src:  "func f() {\n\t[x, _, ...rest] := xs\n}",
			want: "func f() {\n\tx, _, rest := xs[0], xs[1], xs[2:]\n}",
		},
		{
			name:   "destructuring temp avoids existing names",
			src:    "func f() { tmp := 1; [x] := g(tmp) }",
			want:   "func f() { tmp := 1; tmp2 := g(tmp); x := tmp2[0] }",
			splits: []int{len("func f() { tmp := 1; tmp2 := g(tmp)")},
		},
		{
			name: "let object destructuring",
			src:  "func f() {\n\tlet {a, b: c} = obj\n}",
			want: "func f() {\n\ta, c := obj.a, obj.b\n}",
		},
		{
			name:   "const object destructuring",
			src:    "func f() { const {A, B: b} = p }",
			want:   "func f() { A, b := p.A, p.B }",
			consts: []int{len("func f() { "), len("func f() { A, ")},
		},
		{
			name:   "const array destructuring hoists call",
			src:    "func f() {\n\tconst [x, _, y] = load()\n}",
			want:   "func f() {\n\ttmp := load(); x, _, y := tmp[0], tmp[1], tmp[2]\n}",
			splits: []int{len("func f() {\n\ttmp := load()")},
			consts: []int{len("func f() {\n\ttmp := load(); "), len("func f() {\n\ttmp := load(); x, _, ")},
		},
		{
			name:    "comma-ok map index",
			src:     "func f() {\n\tconst [v, ok] = m[key(k)]\n}",
			want:    "func f() {\n\tv, ok := m[key(k)]\n}",
			consts:  []int{len("func f() {\n\t"), len("func f() {\n\tv, ")},
			commaOK: []int{len("func f() {\n\t")},
		},
		{
			name:    "comma-ok type assertion",
			src:     "func f() {\n\tlet [n, _] = x.y.(int)\n}",
			want:    "func f() {\n\tn, _ := x.y.(int)\n}",
			commaOK: []int{len("func f() {\n\t")},
		},
		{
			name:    "comma-ok receive assigns",
			src:     "func f() {\n\t[msg, open] = <-c.chans[0]\n}",
			want:    "func f() {\n\tmsg, open = <-c.chans[0]\n}",
			commaOK: []int{len("func f() {\n\t")},
		},
		{
			name:    "comma-ok right side keeps gosling syntax",
			src:     "func f() {\n\tconst [v, ok] = m[t`${a}`]\n}",
			want:    "func f() {\n\tv, ok := m[__goslingT(``, a, ``)]\n}",
			consts:  []int{len("func f() {\n\t"), len("func f() {\n\tv, ")},
			commaOK: []int{len("func f() {\n\t")},
		},
		{
			name:   "slice expression is not comma-ok",
			src:    "func f() {\n\tlet [a, b] = xs[1:]\n}",
			want:   "func f() {\n\ttmp := xs[1:]; a, b := tmp[0], tmp[1]\n}",
			splits: []int{len("func f() {\n\ttmp := xs[1:]")},
		},
		{
			name:   "index in a larger expression is not comma-ok",
			src:    "func f() {\n\tlet [a, b] = *ps[0]\n}",
			want:   "func f() {\n\ttmp := *ps[0]; a, b := tmp[0], tmp[1]\n}",
			splits: []int{len("func f() {\n\ttmp := *ps[0]")},
		},
		{
			name:   "three names are not comma-ok",
			src:    "func f() {\n\tlet [a, b, c] = m[k]\n}",
			want:   "func f() {\n\ttmp := m[k]; a, b, c := tmp[0], tmp[1], tmp[2]\n}",
			splits: []int{len("func f() {\n\ttmp := m[k]")},
		},
		{
			name: "block statement is not destructuring",
			src:  "func f() {\n\t{\n\t\ta()\n\t}\n}",
			want: "func f() {\n\t{\n\t\ta()\n\t}\n}",
		},
		{
			name:   "for of",
			src:    "for (const item of items) { use(item) }",
			want:   "for _, item := range items { use(item) }",
			consts: []int{len("for _, ")},
		},
		{
			name: "for in",
			src:  "for (let k in m) { use(k) }",
			want: "for k := range m { use(k) }",
		},
		{
			name:   "for of composite literal keeps parens",
			src:    "for (const n of []int{1, 2}) { use(n) }",
			want:   "for _, n := range ([]int{1, 2}) { use(n) }",
			consts: []int{len("for _, ")},
		},
		{
			name: "c-style for with let",
			src:  "for (let i = 0; i < n; i++) { use(i) }",
			want: "for i := 0; i < n; i++ { use(i) }",
		},
		{
			name:   "c-style for with const",
			src:    "for (const i = 0; i < n; i++) { }",
			want:   "for i := 0; i < n; i++ { }",
			consts: []int{len("for ")},
		},
		{
			name: "c-style for without declaration",
			src:  "for (; i < n; i++) { }",
			want: "for ; i < n; i++ { }",
		},
		{
			name: "parenthesized for condition untouched",
			src:  "for (a < b) { }",
			want: "for (a < b) { }",
		},
		{
			name: "if with let",
			src:  "if let x = f(); x > 0 { }",
			want: "if x := f(); x > 0 { }",
		},
		{
			name:   "if with const",
			src:    "if const x = f(); x > 0 { }",
			want:   "if x := f(); x > 0 { }",
			consts: []int{len("if ")},
		},
		{
			name: "type switch with let",
			src:  "switch let v = x.(type) { }",
			want: "switch v := x.(type) { }",
		},
		{
			name: "select case with let",
			src:  "select { case let v, ok = <-ch: use(v, ok) }",
			want: "select { case v, ok := <-ch: use(v, ok) }",
		},
		{
			name:   "range with const",
			src:    "for const k, v = range m { }",
			want:   "for k, v := range m { }",
			consts: []int{len("for "), len("for k, ")},
		},
		{
			name: "try inside string and comment untouched",
			src:  "s := \"try { x }?\" // try { }?\n",
			want: "s := \"try { x }?\" // try { }?\n",
		},
		{
			name: "let in function",
			src:  "func f() {\n\tlet x = 1\n}",
			want: "func f() {\n\tx := 1\n}",
		},
		{
			name: "let with type",
			src:  "func f() {\n\tlet x: int64 = 1\n}",
			want: "func f() {\n\tvar x int64 = 1\n}",
		},
		{
			name: "let with type only",
			src:  "func f() {\n\tlet names: []string\n}",
			want: "func f() {\n\tvar names []string\n}",
		},
		{
			name: "let with several names",
			src:  "func f() { let a, b = g() }",
			want: "func f() { a, b := g() }",
		},
		{
			name: "let at package level",
			src:  "let x = f()\n",
			want: "var x = f()\n",
		},
		{
			name: "const with type",
			src:  "func f() {\n\tconst x: int = 1\n}",
			want: "func f() {\n\tconst x int = 1\n}",
		},
		{
			name: "const without type untouched",
			src:  "const x = 1\nconst (\n\ty = 2\n)",
			want: "const x = 1\nconst (\n\ty = 2\n)",
		},
		{
			name: "let as identifier",
			src:  "let := 1\nx := let + 2",
			want: "let := 1\nx := let + 2",
		},
		{
			name: "try as identifier",
			src:  "try := 1\nx := try + 2",
			want: "try := 1\nx := try + 2",
		},
	}
	for _, testCase := range testCases {
		test.Run(testCase.name, func(test *testing.T) {
			result, err := Process([]byte(testCase.src))
			if err != nil {
				test.Fatalf("Process: %v", err)
			}
			if got := string(result.Src); got != testCase.want {
				test.Errorf("got:\n%s\nwant:\n%s", got, testCase.want)
			}
			if len(result.Tries) != len(testCase.tries) {
				test.Fatalf("got %d tries, want %d", len(result.Tries), len(testCase.tries))
			}
			if !slices.Equal(result.Unions, testCase.unions) {
				test.Errorf("unions %+v, want %+v", result.Unions, testCase.unions)
			}
			if !slices.Equal(result.Coalesce, testCase.coalesce) {
				test.Errorf("coalesce %v, want %v", result.Coalesce, testCase.coalesce)
			}
			if !slices.Equal(result.Consts, testCase.consts) {
				test.Errorf("consts %v, want %v", result.Consts, testCase.consts)
			}
			if !slices.Equal(result.CommaOK, testCase.commaOK) {
				test.Errorf("comma-ok %v, want %v", result.CommaOK, testCase.commaOK)
			}
			if !slices.Equal(result.Splits, testCase.splits) {
				test.Errorf("splits %v, want %v", result.Splits, testCase.splits)
			}
			for tryIndex := range testCase.tries {
				if result.Tries[tryIndex] != testCase.tries[tryIndex] {
					test.Errorf("try %d: got %+v, want %+v", tryIndex, result.Tries[tryIndex], testCase.tries[tryIndex])
				}
			}
		})
	}
}

func TestProcessErrors(test *testing.T) {
	testCases := []struct {
		name        string
		src         string
		wantMessage string
		wantOffset  int
	}{
		{"missing catch", "try { g() }\nx()", "try block must be followed by catch or finally", len("try { g() ")},
		{"finally on next line", "try { g() } catch {\n}\nfinally { }", "finally must be on the same line", len("try { g() } catch {\n}\n")},
		{"catch on next line", "try { g() }\ncatch (e) { }", "catch must be on the same line as the closing brace of try", len("try { g() }\n")},
		{"unterminated interpolation", "s := t`a ${x`", "unterminated ${ in template literal", len("s := t`a ")},
		{"empty interpolation", "s := t`a ${}`", "empty ${} in template literal", len("s := t`a ")},
		{"object rest", "func f() {\n{a, ...rest} := x\n}", "rest elements are only supported in array destructuring", len("func f() {\n{a, ")},
		{"optional index", "x := a?.[0]", "?.[ and ?.( are not supported", len("x := a")},
		{"nullish assignment", "x ??= 1", "??= is not supported", len("x ")},
		{"union inside function", "func f() {\n\ttype S = A | B\n}", "union types must be declared at package level", len("func f() {\n\t")},
		{"generic union", "type R[T any] = Ok[T] | Err\n", "generic union types are not supported", 0},
		{"question mark", "x := f()?", "the ? operator is not supported; use try/catch", len("x := f()")},
		{"question mark before space", "x := f()? .y", "the ? operator is not supported", len("x := f()")},
		{"spread not first", "p := Point{X: 1, ...p}", "struct spread must be the first element", len("p := Point{X: 1, ")},
		{"spread without type", "p := {...q}", "struct spread needs a type name before {", len("p := ")},
		{"let without type or value", "func f() {\n\tlet x\n}", "let x needs a type or a value", len("func f() {\n\t")},
		{"let with keyword after name", "func f() {\n\tlet x := 1\n}", "use let x = value, not :=", len("func f() {\n\t")},
		{"let destructuring at package level", "let {a} = p\n", "destructuring with let or const is only supported in functions", 0},
		{"const destructuring with define", "func f() {\n\tconst {a} := p\n}", "use const {...} = value, not :=", len("func f() {\n\t")},
		{"let with type in header", "if let x: int = 1; x > 0 { }", "let in an if, for, switch, or case header cannot have a type", len("if ")},
		{"bad binding", "try { g() } catch (e error) { }", "expected catch (name) or catch", len("try { g() } ")},
	}
	for _, testCase := range testCases {
		test.Run(testCase.name, func(test *testing.T) {
			_, err := Process([]byte(testCase.src))
			if err == nil {
				test.Fatal("expected error")
			}
			syntaxError, ok := err.(*Error)
			if !ok {
				test.Fatalf("got %T, want *Error", err)
			}
			if !strings.Contains(syntaxError.Msg, testCase.wantMessage) {
				test.Errorf("msg %q does not contain %q", syntaxError.Msg, testCase.wantMessage)
			}
			if syntaxError.Offset != testCase.wantOffset {
				test.Errorf("offset %d, want %d", syntaxError.Offset, testCase.wantOffset)
			}
		})
	}
}

func TestOrigOffset(test *testing.T) {
	src := "try { g() } catch (e) { h(e) }"
	result, err := Process([]byte(src))
	if err != nil {
		test.Fatal(err)
	}
	processed := string(result.Src)
	for _, tokenText := range []string{"g()", "h(e)", "}"} {
		gotOffset := result.OrigOffset(strings.LastIndex(processed, tokenText))
		if want := strings.LastIndex(src, tokenText); gotOffset != want {
			test.Errorf("%s: orig offset %d, want %d", tokenText, gotOffset, want)
		}
	}
	// Offsets inside replaced text map to the start of the original token.
	if got := result.OrigOffset(strings.Index(processed, "true")); got != 0 {
		test.Errorf("inside replacement: got %d, want 0", got)
	}
}

func TestOrigRange(test *testing.T) {
	src := "func f() {\n\tlet n = g()\n\treturn n\n}"
	result, err := Process([]byte(src))
	if err != nil {
		test.Fatal(err)
	}
	processed := string(result.Src)
	for _, name := range []string{"g", "return"} {
		start := strings.Index(processed, name)
		origStart, ok := result.OrigRange(start, start+len(name))
		if !ok {
			test.Fatalf("OrigRange(%q) is not exact", name)
		}
		if got := src[origStart : origStart+len(name)]; got != name {
			test.Errorf("OrigRange(%q) maps to %q", name, got)
		}
	}
	defineStart := strings.Index(processed, ":=")
	if _, ok := result.OrigRange(defineStart, defineStart+len(":=")); ok {
		test.Errorf("OrigRange of generated := is exact, want not exact")
	}
}
