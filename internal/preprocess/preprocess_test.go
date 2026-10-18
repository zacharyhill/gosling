package preprocess

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestProcess(test *testing.T) {
	testCases := []struct {
		name        string
		src         string
		want        string
		tries       []Try
		splits      []int
		coalesce    []int
		unions      []Union
		consts      []int
		forOfBlanks []int
		newlines    [][2]int
		arrayTemps  []ArrayTemp
		params      []int
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
			name: "arrow without spaces",
			src:  "f(x=>x*2, (a,b)=>a+b)",
			want: "f(func(x __goslingU)__goslingR { return x*2 }, func(a __goslingU,b __goslingU)__goslingR { return a+b })",
		},
		{
			name:        "for-of without a space",
			src:         "for(const x of xs){\n}",
			want:        "for _, x := range xs{\n}",
			consts:      []int{len("for _, ")},
			forOfBlanks: []int{len("for ")},
		},
		{
			name: "c-style for without a space",
			src:  "for(let i = 0; i < n; i++){\n}",
			want: "for i := 0; i < n; i++{\n}",
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
			name: "arrow with expression body on the next line",
			src:  "xs.map(x =>\n\tx * 2,\n)",
			want: "xs.__goslingMap(func(x __goslingU) __goslingR { return (\n\tx * 2) },\n)",
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
			name:   "function parameter destructuring",
			src:    "func handle({Name, Port: port}: Config) {\n\tuse(Name, port)\n}",
			want:   "func handle(config Config) { Name, port := config.Name, config.Port;\n\tuse(Name, port)\n}",
			params: []int{len("func handle(config Config) { ")},
		},
		{
			name:   "method parameter destructuring",
			src:    "func (s *S) f(n int, [a, b]: [2]int, {X}: *geo.HTTPPoint) (struct{}, error) { return use(a, b, X) }",
			want:   "func (s *S) f(n int, arg [2]int, httpPoint *geo.HTTPPoint) (struct{}, error) { a, b := arg[0], arg[1]; X := httpPoint.X; return use(a, b, X) }",
			params: []int{len("func (s *S) f(n int, arg [2]int, httpPoint *geo.HTTPPoint) (struct{}, error) { "), len("func (s *S) f(n int, arg [2]int, httpPoint *geo.HTTPPoint) (struct{}, error) { a, b := arg[0], arg[1]; ")},
		},
		{
			name:   "function literal parameter destructuring avoids existing names",
			src:    "f(func({X}: P, p int) { use(X, p) })",
			want:   "f(func(p2 P, p int) { X := p2.X; use(X, p) })",
			params: []int{len("f(func(p2 P, p int) { ")},
		},
		{
			name:   "function literal with result type is not a method",
			src:    "f := func({X}: P) int { return X }",
			want:   "f := func(p P) int { X := p.X; return X }",
			params: []int{len("f := func(p P) int { ")},
		},
		{
			name:   "generic function parameter destructuring",
			src:    "func f[T any]({V}: Box[T]) T { return V }",
			want:   "func f[T any](box Box[T]) T { V := box.V; return V }",
			params: []int{len("func f[T any](box Box[T]) T { ")},
		},
		{
			name:   "untyped arrow parameter destructuring",
			src:    "xs.forEach((i, {X}) => use(i, X))",
			want:   "xs.forEach(func(i __goslingU, arg __goslingU) __goslingR { X := arg.X; return use(i, X) })",
			params: []int{len("xs.forEach(func(i __goslingU, arg __goslingU) __goslingR { ")},
		},
		{
			name:   "typed arrow parameter destructuring with block body",
			src:    "f := ([a, b]: Pair) => {\n\treturn a + b\n}",
			want:   "f := func(pair Pair) __goslingR { a, b := pair[0], pair[1];\n\treturn a + b\n}",
			params: []int{len("f := func(pair Pair) __goslingR { ")},
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
			name: "typed catch clauses",
			src:  "try { a() } catch (e: *fs.PathError) { b(e) } catch (e is ErrX) { c(e) } catch (e) { d(e) }",
			want: "if true { a() } else if __goslingAs(new(*fs.PathError)) { var e *fs.PathError; _ = e; b(e) } else if __goslingIs(ErrX) { var e error; _ = e; c(e) } else { var e error; _ = e; d(e) }",
			tries: []Try{{
				If:       0,
				HasCatch: true,
				CatchEnd: len("if true { a() } else if __goslingAs(new(*fs.PathError)) { var e *fs.PathError; _ = e; b(e) } else if __goslingIs(ErrX) { var e error; _ = e; c(e) } else { var e error; _ = e;"),
				Binding:  "e",
				Filters: []Filter{
					{Binding: "e", CatchEnd: len("if true { a() } else if __goslingAs(new(*fs.PathError)) { var e *fs.PathError; _ = e;")},
					{Binding: "e", CatchEnd: len("if true { a() } else if __goslingAs(new(*fs.PathError)) { var e *fs.PathError; _ = e; b(e) } else if __goslingIs(ErrX) { var e error; _ = e;")},
				},
			}},
		},
		{
			name: "typed catch with interface type, bare catch, and finally",
			src:  "try { a() } catch (t: interface{ Timeout() bool }) { b() } catch { c() } finally { d() }",
			want: "if true { a() } else if __goslingAs(new(interface{ Timeout() bool })) { var t interface{ Timeout() bool }; _ = t; b() } else { c() } ; { d() }",
			tries: []Try{{
				If:       0,
				HasCatch: true,
				CatchEnd: len("if true { a() } else if __goslingAs(new(interface{ Timeout() bool })) { var t interface{ Timeout() bool }; _ = t; b() } else {"),
				Filters:  []Filter{{Binding: "t", CatchEnd: len("if true { a() } else if __goslingAs(new(interface{ Timeout() bool })) { var t interface{ Timeout() bool }; _ = t;")}},
				Finally:  len("if true { a() } else if __goslingAs(new(interface{ Timeout() bool })) { var t interface{ Timeout() bool }; _ = t; b() } else { c() } ; "),
			}},
		},
		{
			name: "only typed catch clauses",
			src:  "try { a() } catch (e is sql.ErrNoRows) { b() }",
			want: "if true { a() } else if __goslingIs(sql.ErrNoRows) { var e error; _ = e; b() }",
			tries: []Try{{
				If:       0,
				HasCatch: true,
				Filters:  []Filter{{Binding: "e", CatchEnd: len("if true { a() } else if __goslingIs(sql.ErrNoRows) { var e error; _ = e;")}},
			}},
		},
		{
			name:  "try finally",
			src:   "try { a() } finally { c() }",
			want:  "if true { a() } ; { c() }",
			tries: []Try{{If: 0, Finally: len("if true { a() } ; ")}},
		},
		{
			name: "try catch catch panic finally",
			src:  "try { a() } catch (e) { b() } catch panic (p) { c() } finally { d() }",
			want: "if true { a() } else if true { var e error; _ = e; b() } else { var p any; _ = p; c() } ; { d() }",
			tries: []Try{{
				If:           0,
				HasCatch:     true,
				CatchEnd:     len("if true { a() } else if true { var e error; _ = e;"),
				Binding:      "e",
				HasPanic:     true,
				PanicEnd:     len("if true { a() } else if true { var e error; _ = e; b() } else { var p any; _ = p;"),
				PanicBinding: "p",
				Finally:      len("if true { a() } else if true { var e error; _ = e; b() } else { var p any; _ = p; c() } ; "),
			}},
		},
		{
			name:  "catch panic only",
			src:   "try { a() } catch panic { c() }",
			want:  "if true { a() } else { c() }",
			tries: []Try{{If: 0, HasPanic: true, PanicEnd: len("if true { a() } else {")}},
		},
		{
			name: "bare catch before catch panic",
			src:  "try { a() } catch { b({}) } catch panic (r) { c() }",
			want: "if true { a() } else if true { b({}) } else { var r any; _ = r; c() }",
			tries: []Try{{
				If:           0,
				HasCatch:     true,
				CatchEnd:     len("if true { a() } else if true {"),
				HasPanic:     true,
				PanicEnd:     len("if true { a() } else if true { b({}) } else { var r any; _ = r;"),
				PanicBinding: "r",
			}},
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
			name: "slice spread after elements",
			src:  "all := [x, y, ...a, z]",
			want: "all := __goslingCat(__goslingOne(x, y), a, __goslingOne(z))",
		},
		{
			name: "slice spread in keyed element",
			src:  "m := map[string][]int{\"k\": [x, ...a]}",
			want: "m := map[string][]int{\"k\": __goslingCat(__goslingOne(x), a)}",
		},
		{
			name: "index and instantiation are not spread",
			src:  "v := xs[i] + m[k][f(a...)] + F[int, string](ys...)",
			want: "v := xs[i] + m[k][f(a...)] + F[int, string](ys...)",
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
			name: "struct spread alone with trailing comma",
			src:  "p2 := Point{...p,\n}",
			want: "p2 := Point(__goslingWith(p,\nPoint{}))",
		},
		{
			name: "address of struct spread",
			src:  "p2 := &geo.Point{...p, X: 5}",
			want: "p2 := __goslingAddr(geo.Point(__goslingWith(p, geo.Point{ X: 5})))",
		},
		{
			name: "struct spread keeps comment and newline before spread",
			src:  "p2 := Point{ // c\n\t...p, X: 5}",
			want: "p2 := Point(__goslingWith( // c\n\tp, Point{ X: 5}))",
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
			name: "multi-line union keeps line count",
			src:  "type Shape = Circle |\n\t*Square\nvar x int",
			want: "type Shape interface{ isShape() }; func (Circle) isShape() {}; func (*Square) isShape() {}; func __goslingVariants_Shape(Circle, *Square) {}\n\nvar x int",
			splits: []int{
				len("type Shape interface{ isShape() }"),
				len("type Shape interface{ isShape() }; func (Circle) isShape() {}"),
			},
			unions: []Union{{
				Name:        "Shape",
				MarkerStart: len("type Shape interface{ isShape() }; func (Circle) isShape() {}; func (*Square) isShape() {}"),
				MarkerEnd:   len("type Shape interface{ isShape() }; func (Circle) isShape() {}; func (*Square) isShape() {}; func __goslingVariants_Shape(Circle, *Square) {}\n"),
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
			want: "func f() {\n\tx, rest := xs[0], xs[2:]\n}",
		},
		{
			name: "array destructuring skips blank elements",
			src:  "func f() {\n\t[_, b, _, ..._] := xs\n}",
			want: "func f() {\n\tb := xs[1]\n}",
		},
		{
			name: "array destructuring with only blank elements",
			src:  "func f() {\n\tlet [_, _] = xs\n}",
			want: "func f() {\n\t_ = xs\n}",
		},
		{
			name:       "array destructuring of a call with only blank elements",
			src:        "func f() {\n\tconst [_] = load()\n}",
			want:       "func f() {\n\ttmp := load(); _ = tmp\n}",
			splits:     []int{len("func f() {\n\ttmp := load()")},
			arrayTemps: []ArrayTemp{{Offset: len("func f() {\n\t"), Names: []string{"_"}}},
		},
		{
			name:       "destructuring temp avoids existing names",
			src:        "func f() { tmp := 1; [x] := g(tmp) }",
			want:       "func f() { tmp := 1; tmp2 := g(tmp); x := tmp2[0] }",
			splits:     []int{len("func f() { tmp := 1; tmp2 := g(tmp)")},
			arrayTemps: []ArrayTemp{{Offset: len("func f() { tmp := 1; "), Names: []string{"x"}}},
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
			name:       "const array destructuring hoists call",
			src:        "func f() {\n\tconst [x, _, y] = load()\n}",
			want:       "func f() {\n\ttmp := load(); x, y := tmp[0], tmp[2]\n}",
			splits:     []int{len("func f() {\n\ttmp := load()")},
			consts:     []int{len("func f() {\n\ttmp := load(); "), len("func f() {\n\ttmp := load(); x, ")},
			arrayTemps: []ArrayTemp{{Offset: len("func f() {\n\t"), Names: []string{"x", "_", "y"}}},
		},
		{
			name: "nested object destructuring",
			src:  "func f() {\n\tlet {Addr: {Host, Port: port}, Name} = cfg\n}",
			want: "func f() {\n\tHost, port, Name := cfg.Addr.Host, cfg.Addr.Port, cfg.Name\n}",
		},
		{
			name:   "nested array destructuring hoists call",
			src:    "func f() {\n\tconst [[a, b], c, [...rest]] = load()\n}",
			want:   "func f() {\n\ttmp := load(); a, b, c, rest := tmp[0][0], tmp[0][1], tmp[1], tmp[2][0:]\n}",
			splits: []int{len("func f() {\n\ttmp := load()")},
			consts: []int{
				len("func f() {\n\ttmp := load(); "),
				len("func f() {\n\ttmp := load(); a, "),
				len("func f() {\n\ttmp := load(); a, b, "),
				len("func f() {\n\ttmp := load(); a, b, c, "),
			},
		},
		{
			name: "mixed nested destructuring",
			src:  "func f() {\n\tlet {Points: [{X}, _, last]} = shape\n}",
			want: "func f() {\n\tX, last := shape.Points[0].X, shape.Points[2]\n}",
		},
		{
			name: "array of objects destructuring assigns",
			src:  "func f() {\n\t[{X: x}, {Y: y}] = points\n}",
			want: "func f() {\n\tx, y = points[0].X, points[1].Y\n}",
		},
		{
			name: "block with labeled block is not destructuring",
			src:  "func f() {\n\t{L: {x()}}\n}",
			want: "func f() {\n\t{L: {x()}}\n}",
		},
		{
			name:   "array destructuring with rest has no array temp",
			src:    "func f() {\n\t[x, ...rest] = load()\n}",
			want:   "func f() {\n\ttmp := load(); x, rest = tmp[0], tmp[1:]\n}",
			splits: []int{len("func f() {\n\ttmp := load()")},
		},
		{
			name: "object destructuring defaults",
			src:  "func f() {\n\tlet {Port = 8080, Name: name = g(a, b), Host} = cfg\n}",
			want: "func f() {\n\tPort, name, Host := __goslingDefault(cfg.Port, 8080), __goslingDefault(cfg.Name, g(a, b)), cfg.Host\n}",
		},
		{
			name:   "array destructuring defaults hoist call",
			src:    "func f() {\n\tconst [a, b = []int{1, 2}] = load()\n}",
			want:   "func f() {\n\ttmp := load(); a, b := tmp[0], __goslingDefaultAt(len(tmp) > 1, tmp[1], []int{1, 2})\n}",
			splits: []int{len("func f() {\n\ttmp := load()")},
			consts: []int{len("func f() {\n\ttmp := load(); "), len("func f() {\n\ttmp := load(); a, ")},
		},
		{
			name: "block with assignment is not destructuring",
			src:  "func f() {\n\t{x = 1}\n}",
			want: "func f() {\n\t{x = 1}\n}",
		},
		{
			name: "block statement is not destructuring",
			src:  "func f() {\n\t{\n\t\ta()\n\t}\n}",
			want: "func f() {\n\t{\n\t\ta()\n\t}\n}",
		},
		{
			name: "block of expression statements is not destructuring",
			src:  "func f() {\n\t{\n\t\ta\n\t\tb()\n\t}\n}",
			want: "func f() {\n\t{\n\t\ta\n\t\tb()\n\t}\n}",
		},
		{
			name:     "multi-line object pattern with trailing comma",
			src:      "func f() {\n\tconst {\n\t\tName,\n\t\tPort: port,\n\t} = cfg\n\tg()\n}",
			want:     "func f() {\n\tName, port := cfg.Name, cfg.Port\n\n\n\n\tg()\n}",
			consts:   []int{len("func f() {\n\t"), len("func f() {\n\tName, ")},
			newlines: [][2]int{{len("func f() {\n\tName, port := cfg.Name, cfg.Port"), len("func f() {\n\tName, port := cfg.Name, cfg.Port\n\n\n")}},
		},
		{
			name: "array pattern with trailing comma",
			src:  "func f() {\n\tlet [a, b,] = xs\n}",
			want: "func f() {\n\ta, b := xs[0], xs[1]\n}",
		},
		{
			name:     "multi-line array pattern of a call",
			src:      "func f() {\n\tlet [\n\t\ta,\n\t\t...rest\n\t] = load()\n\tg()\n}",
			want:     "func f() {\n\ttmp := load(); a, rest := tmp[0], tmp[1:]\n\n\n\n\tg()\n}",
			splits:   []int{len("func f() {\n\ttmp := load()")},
			newlines: [][2]int{{len("func f() {\n\ttmp := load(); a, rest := tmp[0], tmp[1:]"), len("func f() {\n\ttmp := load(); a, rest := tmp[0], tmp[1:]\n\n\n")}},
		},
		{
			name:        "for of",
			src:         "for (const item of items) { use(item) }",
			want:        "for _, item := range items { use(item) }",
			consts:      []int{len("for _, ")},
			forOfBlanks: []int{len("for ")},
		},
		{
			name:        "for of object destructuring",
			src:         "for (const {Name, Port: port} of configs) {\n\tuse(Name, port)\n}",
			want:        "for _, tmp := range configs { Name, port := tmp.Name, tmp.Port;\n\tuse(Name, port)\n}",
			consts:      []int{len("for _, tmp := range configs { "), len("for _, tmp := range configs { Name, ")},
			forOfBlanks: []int{len("for ")},
		},
		{
			name:        "for of array destructuring with composite literal",
			src:         "for (let [k, v] of [][2]string{{\"a\", \"b\"}}) { use(k, v) }",
			want:        "for _, tmp := range ([][2]string{{\"a\", \"b\"}}) { k, v := tmp[0], tmp[1]; use(k, v) }",
			forOfBlanks: []int{len("for ")},
		},
		{
			name:        "for of destructuring avoids existing names",
			src:         "for (const [tmp] of xs) { use(tmp) }",
			want:        "for _, tmp2 := range xs { tmp := tmp2[0]; use(tmp) }",
			consts:      []int{len("for _, tmp2 := range xs { ")},
			forOfBlanks: []int{len("for ")},
		},
		{
			name: "for in",
			src:  "for (let k in m) { use(k) }",
			want: "for k := range m { use(k) }",
		},
		{
			name:        "for of composite literal keeps parens",
			src:         "for (const n of []int{1, 2}) { use(n) }",
			want:        "for _, n := range ([]int{1, 2}) { use(n) }",
			consts:      []int{len("for _, ")},
			forOfBlanks: []int{len("for ")},
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
			name:   "type switch with const after init",
			src:    "switch let v = f(); const s = v.(type) { }",
			want:   "switch v := f(); s := v.(type) { }",
			consts: []int{len("switch v := f(); ")},
		},
		{
			name: "type switch with let after init with call",
			src:  "switch let v = g(a, b); let s = v.(type) { }",
			want: "switch v := g(a, b); s := v.(type) { }",
		},
		{
			name: "const after a statement stays",
			src:  "func f() {\n\tg(); const x = 1\n}",
			want: "func f() {\n\tg(); const x = 1\n}",
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
		{
			name: "annotated parameters and result",
			src:  "func f(a: int, b: string): bool { return true }",
			want: "func f(a int, b string) bool { return true }",
		},
		{
			name: "annotated method, generics, and variadic",
			src:  "func (u: *User) Map[T any](xs: ...T): []T {}",
			want: "func (u *User) Map[T any](xs ...T) []T {}",
		},
		{
			name: "annotated result list",
			src:  "func f(): (n: int, err: error) {}\nfunc g(): (int, error) {}",
			want: "func f() (n int, err error) {}\nfunc g() (int, error) {}",
		},
		{
			name: "annotated func literal and func type",
			src:  "func f() {\n\tlet g: func(n: int): bool = func(n: int): bool { return n > 0 }\n}",
			want: "func f() {\n\tvar g func(n int) bool = func(n int) bool { return n > 0 }\n}",
		},
		{
			name: "annotated parameter with nested func type",
			src:  "func apply(f: func(int) int, x: int): int {}",
			want: "func apply(f func(int) int, x int) int {}",
		},
		{
			name: "go parameter order untouched",
			src:  "func (User) f(a, b int) (int, error) {}",
			want: "func (User) f(a, b int) (int, error) {}",
		},
		{
			name: "annotated interface methods",
			src:  "type S interface {\n\tArea(): float64\n\tScale(by: float64)\n\tfmt.Stringer\n}",
			want: "type S interface {\n\tArea() float64\n\tScale(by float64)\n\tfmt.Stringer\n}",
		},
		{
			name: "annotated arrow parameters",
			src:  "f := (x: int, y: int) => x + y",
			want: "f := func(x int, y int) __goslingR { return x + y }",
		},
		{
			name: "arrow with an annotated func parameter",
			src:  "f := (g func(n: int) int) => g(1)",
			want: "f := func(g func(n int) int) __goslingR { return g(1) }",
		},
		{
			name: "func type in a case clause",
			src:  "switch v.(type) {\ncase func(a: int):\n\tg()\n}",
			want: "switch v.(type) {\ncase func(a int):\n\tg()\n}",
		},
		{
			name: "throw",
			src:  "func f() error {\n\tthrow err\n}",
			want: "func f() error {\n\tpanic(__goslingThrow(err))\n}",
		},
		{
			name: "throw with call in block",
			src:  "if bad { throw fmt.Errorf(\"x: %w\", e) }",
			want: "if bad { panic(__goslingThrow(fmt.Errorf(\"x: %w\", e))) }",
		},
		{
			name: "throw with composite literal and arrow",
			src:  "throw &E{f: x => x}\n",
			want: "panic(__goslingThrow(&E{f: func(x __goslingU) __goslingR { return x }}))\n",
		},
		{
			name: "throw as identifier",
			src:  "throw(x)\nthrow = 1\nthrow.Do()\ny := throw",
			want: "throw(x)\nthrow = 1\nthrow.Do()\ny := throw",
		},
		{
			name: "try expression",
			src:  "n := try strconv.Atoi(s) catch 0",
			want: "n := __goslingTry(strconv.Atoi(s) )( 0)",
		},
		{
			name: "try expressions as arguments",
			src:  "f(try g(a, b) catch x + 1, try h() catch T{1})",
			want: "f(__goslingTry(g(a, b) )( x + 1), __goslingTry(h() )( T{1}))",
		},
		{
			name: "try expression in fallback",
			src:  "return try a() catch try b() catch 0, nil\n",
			want: "return __goslingTry(a() )( __goslingTry(b() )( 0)), nil\n",
		},
		{
			name: "try expression in try block",
			src:  "try {\n\tn := try a(func() { b() }) catch 0\n} catch {\n}",
			want: "if true {\n\tn := __goslingTry(a(func() { b() }) )( 0)\n} else {\n}",
			tries: []Try{{
				If:       0,
				HasCatch: true,
				CatchEnd: len("if true {\n\tn := __goslingTry(a(func() { b() }) )( 0)\n} else {"),
			}},
		},
		{
			name: "try without catch stays an identifier",
			src:  "x := try(a)\ny := try.b",
			want: "x := try(a)\ny := try.b",
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
			if !slices.Equal(result.ForOfBlanks, testCase.forOfBlanks) {
				test.Errorf("forOfBlanks %v, want %v", result.ForOfBlanks, testCase.forOfBlanks)
			}
			if !slices.Equal(result.Newlines, testCase.newlines) {
				test.Errorf("newlines %v, want %v", result.Newlines, testCase.newlines)
			}
			if !slices.EqualFunc(result.ArrayTemps, testCase.arrayTemps, func(got, want ArrayTemp) bool {
				return got.Offset == want.Offset && slices.Equal(got.Names, want.Names)
			}) {
				test.Errorf("array temps %v, want %v", result.ArrayTemps, testCase.arrayTemps)
			}
			if !slices.Equal(result.Params, testCase.params) {
				test.Errorf("params %v, want %v", result.Params, testCase.params)
			}
			if !slices.Equal(result.Splits, testCase.splits) {
				test.Errorf("splits %v, want %v", result.Splits, testCase.splits)
			}
			for tryIndex := range testCase.tries {
				if !reflect.DeepEqual(result.Tries[tryIndex], testCase.tries[tryIndex]) {
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
		{"nested object rest", "func f() {\n\tlet [{a, ...rest}] = x\n}", "rest elements are only supported in array destructuring", len("func f() {\n\tlet [{a, ")},
		{"rest default", "func f() {\n\tlet [a, ...rest = xs] = ys\n}", "a rest element cannot have a default", len("func f() {\n\tlet [a, ...rest ")},
		{"template default", "func f() {\n\tlet {Name = t`x`} = c\n}", "a destructuring default must be plain Go", len("func f() {\n\tlet {Name = ")},
		{"nullish default", "func f() {\n\tlet {Name = a ?? b} = c\n}", "a destructuring default must be plain Go", len("func f() {\n\tlet {Name = a ")},
		{"for in destructuring", "for (const [k, v] in m) { }", "destructuring in a loop needs for...of", len("for (const ")},
		{"parameter pattern without body", "type F func({X}: P)\n", "a destructured parameter needs a function body", len("type F func(")},
		{"parameter pattern without type", "func f({X}) {\n}", "a destructured parameter needs a type", len("func f(")},
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
		{"mixed parameter orders", "func f(a: int, b string) {}", "write name: type for every parameter", len("func f(a: int, ")},
		{"grouped annotated parameters", "func f(a, b: int) {}", "write name: type for every parameter", len("func f(")},
		{"go parameters with annotated result", "func f(a int): bool {}", "write name: type for every parameter", len("func f(")},
		{"annotated parameters with go result", "func f(a: int) bool {}", "write ): before the result type", len("func f(a: int) ")},
		{"annotated parameters with go result list", "func f(a: int) (int, error) {}", "write ): before the result type", len("func f(a: int) ")},
		{"mixed result list", "func f(): (n: int, error) {}", "write name: type for every parameter", len("func f(): (n: int, ")},
		{"mixed arrow parameters", "f := (a, b: int) => a", "write name: type for every parameter", len("f := (")},
		{"throw without value", "func f() error {\n\tthrow\n}", "throw needs an error value", len("func f() error {\n\t")},
		{"bad binding", "try { g() } catch (e error) { }", "expected catch, catch (name), catch (name: Type), or catch (name is value)", len("try { g() } ")},
		{"catch after catch all", "try { g() } catch (e) { } catch (e is X) { }", "only the last catch can catch all errors", len("try { g() } catch (e) { } ")},
		{"catch on next line after catch", "try { g() } catch (e is X) { }\ncatch (e) { }", "catch must be on the same line as the closing brace before it", len("try { g() } catch (e is X) { }\n")},
		{"catch type on two lines", "try { g() } catch (e: interface{\n}) { }", "a catch type must be on one line", len("try { g() } catch (e: ")},
		{"try expression with catch binding", "n := try g() catch (e) => 0", "a try expression cannot use the error; use a try statement", len("n := try g() ")},
		{"catch after catch panic", "try { g() } catch panic { } catch (e) { }", "catch panic must be the last catch clause", len("try { g() } catch panic { } ")},
		{"two catch blocks", "try { g() } catch { } catch (e) { }", "only the last catch can catch all errors", len("try { g() } catch { } ")},
		{"catch panic after typed catch", "try { g() } catch (e is X) { } catch panic { }", "catch panic cannot follow a typed catch clause", len("try { g() } catch (e is X) { } ")},
		{"catch panic on next line", "try { g() } catch {\n}\ncatch panic { }", "catch must be on the same line as the closing brace before it", len("try { g() } catch {\n}\n")},
		{"bad panic binding", "try { g() } catch panic (p any) { }", "expected catch panic (name) or catch panic", len("try { g() } ")},
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

func TestProcessReserving(test *testing.T) {
	result, err := ProcessReserving([]byte("func f() { [x] := g() }"), map[string]bool{"tmp": true})
	if err != nil {
		test.Fatal(err)
	}
	if want := "func f() { tmp2 := g(); x := tmp2[0] }"; string(result.Src) != want {
		test.Errorf("got %q, want %q", result.Src, want)
	}
	if len(result.TempNames) != 1 || result.TempNames[0] != "tmp2" {
		test.Errorf("TempNames = %v, want [tmp2]", result.TempNames)
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
