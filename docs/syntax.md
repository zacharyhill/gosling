# gosling syntax guide

This guide describes the syntax of `.gol` files. It is for people and for AI agents that write `.gol` code. The text uses Simplified Technical English (ASD-STE100).

## Terms

| Term         | Meaning                                                                                   |
| ------------ | ----------------------------------------------------------------------------------------- |
| `.gol` file  | A source file that you write. It contains Go code and gosling syntax.                     |
| output       | The `.go` file that gosling generates from a `.gol` file.                                 |
| checked call | A call in a try block whose last result is an error. See [Checked calls](#checked-calls). |
| failure      | A checked call that returns an error that is not nil.                                     |
| context      | The type that the code around an expression expects, for example a parameter type.        |
| variant      | One of the types or values that a union type lists.                                       |

## Basic rules

1. A `.gol` file is a Go file with more syntax. All Go syntax is valid, except for the items in rule 2 and rule 3.
2. Do not use `:=`. Use `let` or `const`. The error is `use let or const instead of :=`.
3. Do not use `var` in a function. Use `let`. The error is `use let instead of var in a function`.
4. Edit only `.gol` files. Do not edit the output. gosling replaces the output on each run.
5. After each change, run `gosling ./...` from the module root.
6. gosling adds the imports that the output needs, for example `fmt`, `strconv`, `strings`, `slices`, `maps`, `errors`, and `regexp`.
7. Some features need a new Go version. Slice spread needs Go 1.22. Generators need Go 1.23.

## Commands

```sh
gosling ./...                         # generate the output for all packages
gosling -check ./...                  # exit 1 if an output file is out of date
gosling -lines=false ./...            # do not add line directives
gosling fmt -w ./...                  # format .gol files
gosling fmt -l ./...                  # list .gol files that are not formatted
gosling lint golangci-lint run ./...  # show linter positions in .gol files
```

- `gosling fmt` changes only the space between tokens. It does not sort imports. It does not format a file that has syntax errors.
- gosling reads `//go:build` lines and `_GOOS` and `_GOARCH` file name suffixes in `.gol` files. The output keeps the `//go:build` line.

## Quick reference

| Feature              | `.gol` syntax                                    | Section                                                       |
| -------------------- | ------------------------------------------------ | ------------------------------------------------------------- |
| Variable             | `let n = 0`, `let names: []string`               | [let and const](#let-and-const)                               |
| Fixed value          | `const data = load()`                            | [let and const](#let-and-const)                               |
| Parameter type order | `func f(name: string): int`                      | [Parameter types](#parameter-types)                           |
| Automatic close      | `using f = os.Open(path)`                        | [using](#using)                                               |
| Error handling       | `try { } catch (e) { } finally { }`              | [try and catch](#try-and-catch)                               |
| Typed catch          | `catch (e: *fs.PathError)`, `catch (e is ErrX)`  | [Typed catch](#typed-catch)                                   |
| try expression       | `try strconv.Atoi(s) catch 0`                    | [try expressions](#try-expressions)                           |
| throw                | `throw fmt.Errorf("bad")`                        | [throw](#throw)                                               |
| Panic recovery       | `catch panic (p) { }`                            | [catch panic](#catch-panic)                                   |
| Template literal     | `` t`hi ${name} ${price:.2f}` ``                 | [Template literals](#template-literals)                       |
| Tagged template      | `` sql`where id = ${id}` ``                      | [Tagged templates](#tagged-templates)                         |
| Regex literal        | `/^\d+$/i`                                       | [Regex literals](#regex-literals)                             |
| Destructuring        | `const {X, Y: y} = p`, `const [a, ...rest] = xs` | [Destructuring](#destructuring)                               |
| Loops                | `for (const x of xs)`, `for (const k in m)`      | [Loops](#loops)                                               |
| Nil handling         | `a?.b`, `a?.[k]`, `x ?? d`, `x ??= d`            | [Optional chaining](#optional-chaining-and-nullish-operators) |
| Ternary              | `c ? a : b`                                      | [Ternary operator](#ternary-operator)                         |
| Type assertion       | `x as T`                                         | [as](#as)                                                     |
| Type check           | `if (x instanceof T) { }`                        | [instanceof](#instanceof)                                     |
| Arrow function       | `(x: int): int => x * 2`, `<T>(x T) => x`        | [Arrow functions](#arrow-functions)                           |
| Default parameter    | `func f(port int = 80, debug? bool)`             | [Default parameters](#default-and-optional-parameters)        |
| Generator            | `func* f() iter.Seq[int] { yield 1 }`            | [Generators](#generators)                                     |
| Slice methods        | `xs.filter(f).map(g)`                            | [Slice and array methods](#slice-and-array-methods)           |
| Map methods          | `m.has(k)`, `m.get(k) ?? 0`                      | [Map methods](#map-methods)                                   |
| String methods       | `s.trim()`, `s.startsWith("a")`                  | [String methods](#string-methods)                             |
| length               | `xs.length`                                      | [length](#length)                                             |
| Spread               | `[...a, x]`, `Point{...p, X: 5}`, `f(...xs)`     | [Spread](#struct-shorthand-and-spread)                        |
| Union type           | `type Shape = Circle \| Square`                  | [Union types](#union-types)                                   |
| Literal union        | `type Method = "GET" \| "POST"`                  | [Literal unions](#literal-unions)                             |
| Enum                 | `enum Color { Red, Green }`                      | [Enums](#enums)                                               |
| Optional field       | `Host? string`                                   | [Optional fields](#optional-fields)                           |
| Method group         | `impl User { constructor() { } }`                | [impl blocks](#impl-blocks)                                   |

## Declarations

### let and const

In a function:

```go
let count = 0                  // count := 0
let names: []string            // var names []string
let port: int64 = 8080         // var port int64 = 8080
let x, y: int                  // var x, y int
const limit = 10               // const limit = 10
const scale: float64 = 1.5     // const scale float64 = 1.5
const data = os.ReadFile(path) // data := os.ReadFile(path)
```

- `let` needs a type, a value, or both.
- Put the type after a colon: `let name: Type`.
- `const` needs a value.
- If the value of a `const` is a Go constant, the output is a Go `const`.
- If the value of a `const` is not a Go constant, the output is a variable. gosling makes sure that the code does not change this variable.

What code can do with a `const` variable:

| Action                                | Allowed |
| ------------------------------------- | ------- |
| Assign a new value: `data = other`    | No      |
| Increment or decrement: `n++`         | No      |
| Use it as a `range` target            | No      |
| Change a field: `cfg.Port = 80`       | Yes     |
| Change an element: `xs[0] = 1`        | Yes     |
| Change the pointed-to value: `*p = v` | Yes     |

The error for a forbidden action is `cannot assign to NAME: declared with const`.

At package level:

```go
let fallback = strings.ToUpper("anon") // var fallback = strings.ToUpper("anon")
let (
	retries = 3                         // var ( retries = 3
	name: string                        //       name string
	low, high: int = 1, 9               //       low, high int = 1, 9 )
)
```

- A `let ( ... )` group is valid only at package level.
- Package-level `var` is also valid.
- `const ( ... )` groups are plain Go. Their values must be Go constants.

### Declarations in a header

The header of an `if`, `for`, `switch`, `select` case, or type switch can start with `let` or `const`. The output uses `:=`.

```go
for (let i = 0; i < n; i++) {               // for i := 0; i < n; i++ {
for const k, v = range m {                  // for k, v := range m {
if let v, ok = m[key]; ok {                 // if v, ok := m[key]; ok {
switch const s = shape.(type) {             // switch s := shape.(type) {
switch let v = f(); const s = v.(type) {    // switch v := f(); s := v.(type) {
case let msg, ok = <-ch:                    // case msg, ok := <-ch:
for (let i: int64 = 0; i < n; i++) {        // for i := int64(0); i < n; i++ {
```

- A header declaration can have a type. The output uses a conversion.
- gosling first makes sure that the value is assignable to the type. So `if let n: int64 = count` with `count int` is an error. Write `int64(count)`.
- A header declaration with a type declares one name. It cannot use `range`, and it cannot be in a `case`.
- Code cannot assign to a name that `const` declares in a header.

### Parameter types

Parameters and results can put the type after a colon, as `let` does:

```go
func parse(text: string, base: int): (int, error) // func parse(text string, base int) (int, error)
func (u: *User) Rename(name: string): string      // func (u *User) Rename(name string) string
let isEven: func(n: int): bool                    // var isEven func(n int) bool
```

- Go order is also valid. So Go code that you copy into a `.gol` file stays valid.
- One function uses one order. If one parameter or the result has a colon, give each parameter and each named result a colon.
- Write `a: int, b: int`. Do not write `a, b: int`.
- In colon order, the result type comes after `):`.
- The receiver can use either order.
- These rules apply to function declarations, function literals, arrow functions, function types, and interface methods.
- In a `case` expression, a function type or a function literal cannot use `):`. Use Go order there.

### using

`using` declares a variable and closes it when the function returns.

```go
try {
	using file = os.Open(path)  // file, err := os.Open(path); check; defer file.Close()
	return io.ReadAll(file)
} catch (e) {
	return nil, e
}
```

- The value must have a `Close` method.
- If the call returns an error, put `using` in a try block. gosling checks the error before the `defer`.
- `using` ignores the error from `Close`. If the error is important, for example for a file that you write, call `Close` and check its error yourself.
- `Close` runs when the function returns, not at the end of the block. So `using` in a loop is an error. Move the loop body into a function.
- `using` in a try block that has `catch panic` is an error.
- Code cannot assign to the variable.
- More than one `using` declaration close in the opposite order.
- `using` declares one name. It cannot have a type or a pattern.
- For a different cleanup, for example `tx.Rollback()`, use `const` and `defer`.

## Errors

### try and catch

```go
try {
	// statements
} catch (e) {
	// e has the type error
} finally {
	// runs on each exit
}
```

- A try statement needs a catch clause, a finally clause, or both. If both are missing, the error is `try block must be followed by catch or finally`.
- Put each `catch` and `finally` on the same line as the `}` before it.
- `catch { ... }` without a variable is valid.
- There is no `?` operator for errors, for example `f()?`. Use try and catch.

### Checked calls

In a try block, gosling checks each call whose last result is an error. A failure goes to the catch block. The catch variable gets the error.

| You write       | Output                                                |
| --------------- | ----------------------------------------------------- |
| `f()`           | `if err := f(); err != nil { e = err; goto catch_N }` |
| `const x = f()` | `x, err := f()`, then `if err != nil { ... }`         |
| `let x = f()`   | `x, err := f()`, then `if err != nil { ... }`         |
| `x = f()`       | `x, e = f()`, then `if e != nil { goto catch_N }`     |
| `x += f()`      | `val, err := f()`, then a check, then `x += val`      |

The declared names do not include the error. So `const n = strconv.Atoi(s)` gives `n` the type `int`.

Which last results count as an error:

- The type `error`.
- A pointer or interface type that implements `error`, for example `*MyError`. A nil `*MyError` is not a failure.
- Other types that implement `error`, for example `type Code int`, do not count. They cannot be nil.

Rules:

- gosling checks all calls that return an error, for example `Close()` and `fmt.Fprintf`.
- To skip the check for one call, assign the error to the blank identifier: `_ = c.Close()`.
- gosling does not change a call that already receives its error, for example `const n, err = f()`.
- Checks apply in nested `if`, `for`, `switch`, and `select` bodies in the try block.
- Checks apply in the init statement of an `if`, `switch`, or `for`, also in an `else if`.
- Checks do not apply in a function literal or an arrow function. Put a new try block in the function.
- Only the last result counts. If an error is not the last result, a call as a statement is an error. Assign the results and check the error yourself.
- An optional call, for example `conn?.Close()`, is checked only if it is a statement.

### Calls outside a try block

Outside a try block, gosling does not check calls. Receive the error, as in Go:

```go
const n, err = strconv.Atoi(s)
if err != nil {
	return 0, err
}
```

If the code does not receive the error, gosling reports an error:

```
strconv.Atoi returns an error that is not checked here; assign the error, or move the call into its own statement in a try block
```

### Calls in expressions

If a checked call is part of a larger expression, gosling moves the call into a new statement. The new statement comes before the original statement.

```go
const n = strconv.Atoi(strings.TrimSpace(s)) * 2

val, err := strconv.Atoi(strings.TrimSpace(s))
if err != nil { ... }
n := val * 2
```

- Moved calls run in the order of the original expression.
- A call that returns `(A, B, error)` moves only in two positions. It must be the only argument of a call, or the only result of a `return`.
- A call that runs only on some paths moves into a new `if` statement. So short-circuit evaluation does not change. This applies to the right side of `&&` and `||`, a loop condition, an `else if` condition, and a `case` expression.

These positions cause an error:

- a `??` default
- after `?.`
- an arm of a ternary that is a value
- the post statement of a `for` loop
- a later expression of a `case`, for example `f()` in `case a, f():`
- a `case` after the `default` case
- a `case` that the case before it enters with `fallthrough`
- the right side of `??=`

To fix the error, move the call into its own statement.

### Control flow

The output uses labeled blocks and `goto`. It does not use closures. So `return`, `break`, and `continue` work as in Go.

- Try blocks can be nested.
- A failure in a catch block goes to the outer try statement.
- The catch variable can have the same name as a variable in the try block.
- If the try block has no checked call, gosling removes the catch block.
- If the try block and each catch block return, the function needs no last `return`.

These calls never return: `t.Fatal`, `t.FailNow`, `t.Skip`, `os.Exit`, `runtime.Goexit`, `log.Fatal`, and `log.Panic`, and their variants, for example `t.Fatalf`. A catch block that ends with one of these calls needs no `return`.

```go
func mustRead(t *testing.T, path string) []byte {
	try {
		const data = os.ReadFile(path)
		return data
	} catch (e) {
		t.Fatal(e)
	}
}
```

### Typed catch

A catch clause can catch only some errors. gosling tests the clauses in order. The first clause that matches runs.

```go
try {
	const data = os.ReadFile(path)
	return parse(data)
} catch (e: *fs.PathError) {
	return t`cannot read ${e.Path}`
} catch (e is fs.ErrNotExist) {
	return "missing"
} catch (e) {
	return e.Error()
}
```

- `catch (e: T)` uses `errors.As`. `e` has the type `T`. `T` must implement `error` or be an interface.
- `catch (e is value)` uses `errors.Is`. `e` is the error.
- Only the last catch clause can catch all errors, with `catch (e)` or `catch`. This clause is optional.
- If no clause matches, the error goes to the outer try statement. The finally block of this try statement runs first.
- If no outer try statement exists, the function returns the error. Then the last result of the function must be `error`. The other results get their zero values.
- Write a catch type on one line.

### try expressions

`try call catch fallback` gives the value of the call. If the call fails, it gives the fallback. gosling discards the error.

```go
const port = try strconv.Atoi(s) catch 8080
const n = (try f() catch 0) + 1
const v = try a() catch try b() catch 0
```

- The call must return exactly one value and an error.
- The fallback runs only if the call fails. It must have the type of the value.
- The fallback continues to the end of the expression. Use parentheses to end it earlier.
- The fallback cannot read the error. `catch (e) => ...` is an error. Use a try statement.
- `try` starts a try expression only if `catch` follows in the same expression. Otherwise `try` is an identifier.

### finally

The finally block runs after the try block or the catch block. It runs on each exit:

- the end of the block
- `return`, `break`, `continue`, or `goto`
- a failure that an outer try statement catches

Rules:

- The finally block does not run on a panic, unless the try statement has a `catch panic` clause.
- gosling evaluates the return values before the finally block runs.
- A `return` in the finally block replaces the pending return.
- Nested finally blocks run from the inside out.
- If the try statement has no catch clause, a failure goes to the outer try statement. The finally block runs first.

### throw

`throw` sends an error to the catch block, as a failure does.

```go
try {
	const n = strconv.Atoi(s)
	if n < 0 {
		throw fmt.Errorf("negative: %d", n)
	}
	return n, nil
} catch (e) {
	throw fmt.Errorf("parse %q: %w", s, e)
}
```

- In a try block, `throw` sets the catch variable and goes to the catch block. The finally blocks between them run first.
- In a catch block, `throw` goes to the outer try statement.
- If no try statement catches the error, the function returns it. The last result of the function must be `error`. So `throw err` in `func f() (int, error)` becomes `return 0, err`.
- In other functions, `throw` outside a try block is an error.
- The value must be an error.
- `throw` is a keyword only at the start of a statement, before a value on the same line.

### catch panic

A `catch panic` clause catches a panic in the try block. Put it after the catch clause, or directly after the try block.

```go
try {
	process(job)
} catch (e) {
	log.Print(e)
} catch panic (p) {
	log.Print("panic: ", p)
} finally {
	job.Done()
}
```

- The variable has the type `any`. It holds the value that `recover` returns.
- `catch panic { ... }` without a variable is valid.
- With `catch panic`, the finally block also runs after a panic in the try block.
- `catch panic` catches only panics in the try block. A panic in a catch block goes up, as in Go.
- `defer` and `using` directly in the try block are errors. A `defer` in a function literal in the try block is valid.
- `runtime.Goexit`, `t.Fatal`, and `os.Exit` are not panics. `catch panic` does not stop them.
- `catch panic` cannot follow a typed catch clause.

## Strings

### Template literals

A template literal starts with `t` and a backtick. Each `${expr}` puts the value of `expr` in the string. The output is a `fmt.Sprintf` call.

```go
const msg = t`hello ${u.Name}, you are ${u.Age}`
// fmt.Sprintf("hello %s, you are %d", u.Name, u.Age)
```

The type of each expression sets the verb:

| Type                                           | Verb or function                      |
| ---------------------------------------------- | ------------------------------------- |
| string                                         | `%s`                                  |
| integer                                        | `%d`                                  |
| bool                                           | `%t`                                  |
| float                                          | `strconv.FormatFloat(x, 'f', -1, 64)` |
| type with methods, for example `time.Duration` | `%v`                                  |
| error                                          | `%v`                                  |
| other types                                    | `%v`                                  |

To set the verb yourself, add a format after a colon:

```go
t`${price:.2f} ${id:05d} ${mask:#x}` // fmt.Sprintf("%.2f %05d %#x", price, id, mask)
```

- A format is a printf verb without the `%`. It can have flags, a width, and a precision.
- The format starts at the first colon outside brackets and quotes. So `${xs[1:3]}` has no format.
- gosling does not check that the verb fits the type. `go vet` does.
- A template literal can have more than one line.
- JavaScript escapes work: `\n`, `\t`, `\\`, `\xHH`, `\uHHHH`, and `\u{H...}`.
- Write `\${` for the text `${`. Write `` \` `` for a backtick.

### Tagged templates

A name before a template literal is a tag. gosling calls the tag function with the text pieces and the values.

```go
const q = sql`select * from users where id = ${id}`
// sql([]string{"select * from users where id = ", ""}, id)
```

- The tag function looks like `func(strings []string, values ...any) R`. The values can have a more specific type, for example `...string`.
- The first argument has one more piece than there are values.
- gosling does not put the values in the text. So a tag such as `sql` can send them as query parameters.
- A tag can be a qualified name, for example `` db.SQL`...` ``.
- `t` is the built-in tag. A function with the name `t` cannot be a tag.
- A tagged template cannot contain an escaped backtick or a format.
- A raw string after a struct field type is a field tag, not a template literal.

### Regex literals

A regex literal becomes a `*regexp.Regexp`.

```go
func isNumber(s string) bool {
	return /^\d+$/.MatchString(s)
}
```

- In a function, gosling moves the `regexp.MustCompile` call to a package-level variable, `re1`, `re2`, and so on. So the pattern compiles one time.
- The pattern uses Go RE2 syntax, not JavaScript syntax. Lookahead and backreferences are not available.
- gosling compiles each pattern. An invalid pattern is an error at the literal.
- The flags `i`, `m`, and `s` are valid: `/a.b/is` becomes `` `(?is)a.b` ``. Other flags, for example `g`, are errors.
- Write `\/` for a `/` in the pattern. A `/` in a character class, for example `[/]`, needs no backslash.
- A `/` after an operand is a division. A `/` at the start of an operand starts a regex literal.
- A regex literal cannot contain a newline.
- In a template literal, a regex literal cannot contain `}`.

## Destructuring

### Patterns

```go
const {X, Y: y} = p             // X, y := p.X, p.Y
const [a, b] = pair             // a, b := pair[0], pair[1]
let [first, _, ...rest] = xs    // first, rest := xs[0], xs[2:]
const {Addr: {Host}} = cfg      // Host := cfg.Addr.Host
const [[a, b], c] = rows        // a, b, c := rows[0][0], rows[0][1], rows[1]
const {Name: name} = load(path) // tmp := load(path); name := tmp.Name
{X, Y} = p                      // X, Y = p.X, p.Y
```

- An object pattern `{Field: name}` reads struct fields. `{Field}` is the same as `{Field: Field}`.
- An array pattern `[a, b]` reads elements of an array or a slice.
- `...rest` gets the remaining elements as a slice. It must be the last element of an array pattern.
- `_` skips an element. gosling does not read its index.
- Patterns can be nested.
- With `let` or `const`, the pattern declares new names. Without them, the pattern assigns to variables that exist.
- Use `let` and `const` patterns only in functions.
- A pattern can have more than one line and a trailing comma.
- Field names must match the struct fields exactly.
- gosling evaluates a complex right side one time.
- An index past the end of a slice panics, as in Go.

### Default values

```go
const {Port = 8080, Timeout = 30} = cfg
const [first = "-"] = args
const {Addr: {Port = 80}} = cfg
```

- In an object pattern, the default applies if the field has its zero value.
- If a pointer field has a default of its element type, the result is the pointed-to value. So `Timeout = 30` with `Timeout *int` gives an `int`.
- In an array pattern, the default applies if the index is past the end of the slice.
- gosling evaluates the default only if it applies.
- A default must be plain Go. Assign gosling syntax, for example a template literal, to a variable first.

### Call results

An array pattern can take the results of a call. The call must have two or more results. A last error does not count:

```go
const [n, name] = parse(s) // parse returns (int, string, error)
```

- In a try block, gosling checks the error.
- A pattern with a rest element, a nested pattern, or a default cannot take call results.

### Parameter patterns

A parameter can be a pattern:

```go
func handle({Name, Port}: Config) { }
configs.map(({Name}) => Name)
```

- gosling names the parameter from the type. `Config` gives `config`. Other types give `arg`.
- In a function declaration or a function literal, write the type after the pattern.
- An arrow parameter without a type gets its type from the context.
- A parameter pattern can be nested and can have defaults.

### Not supported

- A rest element in an object pattern.
- A pattern in a `for...in` loop. Use `for const k, v = range m`.

## Loops

```go
for (const x of xs) { }              // for _, x := range xs { }
for (const k in m) { }               // for k := range m { }
for (let i in 4) { }                 // for i := range 4 { }
for (const v of ch) { }              // for v := range ch { }
for (const {Name, Port} of configs) { }
for (const [k, v] of pairs) { }
for (let i = 0; i < n; i++) { }      // for i := 0; i < n; i++ { }
for const k, v = range m { }         // Go syntax with const
```

- `for...of` gives the values of a slice, an array, a map, or a string.
- For a channel, an integer, or an `iter.Seq`, `for...of` gives each value: `for x := range xs`.
- For an `iter.Seq2`, `for...of` gives the second value. To get both values, write `for const k, v = range seq`.
- `for...in` gives the keys of a map, the indexes of a slice, or the integers below a number.
- A `for...of` loop can destructure each element.
- With `const`, the loop body cannot assign to the loop variable.
- A C-style loop needs parentheses.

## Optional chaining and nullish operators

### Optional chaining: ?.

`a?.b` reads `b` only if `a` is not nil. If a link in the chain is nil, gosling skips the rest of the chain. The result is the zero value of the type.

```go
user?.Profile?.Name   // "" if user or Profile is nil
users[id]?.Profile    // the left side can be any expression
m?.[k]                // skips the chain if the map has no key k
xs?.[i]               // skips the chain if i is out of range
f?.(x)                // calls f only if f is not nil
conn?.Close()         // a statement becomes an if statement
```

- The left side of `?.` must be a pointer or an interface.
- A type parameter is valid only if each type in its constraint can be nil, for example `[P *User]`.
- The left side of `?.[` can be a map, a slice, or a pointer to an array.
- `?.[` cannot start a slice expression. `xs?.[1:]` is an error.
- gosling evaluates each link one time.
- An optional call with zero results or more than one result can only be a statement.
- In a try block, gosling checks the error of an optional call that is a statement. If a link is nil, gosling skips the call and the check.

These are errors:

- the address of a chain: `&a?.b`
- an assignment to a chain or an increment of a chain: `a?.b = c`, `a?.n++`
- a dereference of a chain: `*a?.p`. Use `a?.p ?? d`.

### Nullish coalescing: ??

`x ?? d` gives `d` if `x` is nil or if its chain was skipped. Otherwise it gives `x`.

```go
const name = user?.Profile?.Name ?? "anon"
const port = cfg.Port ?? 8080       // Port is *int, so port is int
const r = reader ?? fallback        // interface values
const n = a ?? b ?? c ?? 0          // a chain of fallbacks
const p = ports?.[name] ?? 8080     // a map lookup with a default
```

- If `x` is a pointer and `d` has the element type, gosling dereferences `x`.
- If `x` is a pointer and `d` is a pointer, the result is a pointer.
- The left side of `??` must be able to be nil. Otherwise the error is `left side of ?? is never nil`.
- Do not mix `??` with `||` or `&&` without parentheses.

### Nullish assignment: ??=

`x ??= d` assigns `d` to `x` only if `x` is nil.

```go
cfg.Port ??= new(8080)
headers[key] ??= []string{}
```

- `x` can be a pointer, a map, a slice, an interface, a func, or a chan.
- A map element is nil if the map has no such key.
- gosling evaluates `x` one time, and `d` only if `x` is nil.
- `??=` does not take an address. If `x` is a pointer, `d` must also be a pointer, for example `&port` or `new(8080)`.
- `??=` is a statement. It cannot be in a header.

## Ternary operator

`c ? a : b` gives `a` if `c` is true. Otherwise it gives `b`. Only the chosen arm runs.

```go
const label = n < 0 ? "negative" : n == 0 ? "zero" : "positive"
ok ? save(doc) : log.Println("skipped") // if ok { save(doc) } else { log.Println("skipped") }
```

Result type:

- An untyped constant gets the type of the other arm.
- If both arms are untyped constants, the result gets the type that the context expects. Without a context, it gets the default type, as in Go.
- If the arms have different types, the result gets the type that the context expects: `let s: Shape = round ? Circle{} : Square{}`. Without a context, one arm must be assignable to the type of the other arm.

Rules:

- The condition must be a `bool`.
- A ternary has the lowest precedence. So `a || b ? x : y` tests `a || b`.
- Nested ternaries group from the right.
- A line cannot start with the `?` or the `:` of a ternary. To split a ternary over lines, end each line with `?` or `:`.
- A ternary that is a statement becomes an `if` statement. Its arms can be calls with no result.
- In a template literal, a colon after the else arm starts a format.
- In a try block, a checked call in a ternary arm is an error if the ternary is a value.

## Types and type checks

### as

`x as T` becomes the type assertion `x.(T)`. As in Go, it panics if `x` does not hold a `T`.

```go
const n = value as int + 1              // n := value.(int) + 1
const [reader, ok] = value as io.Reader // reader, ok := value.(io.Reader)
const s, isString = value as string     // s, isString := value.(string)
const r = (s as Circle).R
```

- `as` applies only to the operand directly before it. If an operator comes before that operand, it is an error. Write `(a + b) as T`.
- The result of `as` is an operand. So `x as T + 1` adds to the result.
- To use a field of the result, add parentheses: `(s as Circle).R`.
- In other positions, `as` is a name, as in Go.
- After `??`, `as` is an error.

### instanceof

`x instanceof T` checks the dynamic type of `x`. In an `if` condition, it also changes the type of `x` to `T` in the if block.

```go
if (s instanceof Circle) {          // if s, ok := s.(Circle); ok {
	return math.Pi * s.R * s.R
}
if (err instanceof *fs.PathError) { // uses errors.AsType
	return err.Path
}
```

- If `x` is an error and `T` implements `error`, gosling uses `errors.AsType`. So the check also finds wrapped errors.
- The condition must be `x instanceof T`, `(x instanceof T)`, or `!(x instanceof T)`. `instanceof` in other positions is an error.
- `x` must be a variable name.
- `!(x instanceof T)` does not change the type of `x`.
- An `else` branch sees the original `x`.
- Code cannot assign to `x` in the if block.

## Functions

### Arrow functions

```go
const double = (x: int) => x * 2                // func(x int) int { return x * 2 }
const add = (a int, b int) => a + b             // Go order is also valid
sort.Slice(xs, (i, j) => xs[i] < xs[j])         // types from the context
const label = (n: int): string => strconv.Itoa(n)
const check = (n: int): (int, error) => {
	if n < 0 {
		return 0, fmt.Errorf("negative: %d", n)
	}
	return n, nil
}
```

Parameter types. A parameter without a type gets its type from the context:

- a slice, array, or map method callback
- a function argument, also of a generic function such as `slices.SortFunc`
- a variable or an assignment with a function type
- a `return` in a function that returns a function type
- an element of a composite literal, a map value, or a struct field
- the first use of the variable that the arrow declares, in a function

```go
const inc = x => x + 1        // x is int, from the first use
const answer = inc(41)
```

- gosling does not use the body to find parameter types.
- Without a context, write the parameter types. Otherwise the error is `cannot infer the parameter types of this arrow function; add types, as in (x int) => ...`.

Result types:

- The result type comes from the declared type, the context, or the body, in this order.
- To declare the result type, write it after `):`. Then use colon order for the parameters: `(x: int): string`.
- Give each parameter a type, or give no parameter a type. `(a: int, b): bool` is an error.
- With a result type, put the parameters in parentheses: `(x): int => x`.
- If the context has no result, an expression body becomes a statement. So `names.forEach(n => fmt.Println(n))` is valid.
- If the body returns only `nil`, the type must come from the context: `let f: func() error = () => nil`.

Generic arrows:

```go
const identity = <T>(x T) => x                        // func identity[T any](x T) T
const keys = <K comparable, V>(m map[K]V) => { ... }  // func keys[K comparable, V any](...)
```

- Only a package-level `const` with one name can hold a generic arrow.
- If the last type parameter has no constraint, it gets `any`.
- The parameters must have types.
- A generic arrow cannot call itself.

Other rules:

- An arrow function can return an arrow function: `(a: int) => (b: int) => a + b`.
- A try block in an arrow function body is valid.
- An expression body can start on the next line after `=>`.
- Arrow functions cannot be generators.

### Default and optional parameters

A parameter of a function or method declaration can have a default value. A parameter with `?` after its name is optional. Its default value is the zero value of its type.

```go
func Dial(addr string, timeout time.Duration = 5*time.Second, port? int) Conn

Dial("db")                   // Dial("db", dialTimeoutDefault(), 0)
Dial("db", time.Second)      // Dial("db", time.Second, 0)
Dial("db", time.Second, 543)
```

- Parameters with default values come last.
- Each parameter with a default needs its own name and type. Write `a int = 1, b int = 2`. Do not write `a, b int = 1`.
- In colon order, write `timeout: time.Duration = time.Second` and `port?: int`.
- Each call evaluates the default value again.
- A default value can use package-level names and imports. It cannot use other parameters.
- A default value must be plain Go.
- Only calls in `.gol` files of the same package can omit arguments.
- A call through a function value or an interface must give all arguments.
- Default values are not supported in generic functions, variadic functions, function literals, arrow functions, and interface methods.

### Generators

A function declared with `func*` is a generator. It returns a Go 1.23 iterator. Each `yield` statement gives one value to the loop that uses the iterator.

```go
func* walk(t *Tree) iter.Seq[int] {
	if t == nil {
		return
	}
	yield* walk(t.Left)
	yield t.Value
	yield* walk(t.Right)
}

for (const v of walk(tree)) {
	fmt.Println(v)
}
```

- The result type must be `iter.Seq[T]` or `iter.Seq2[K, V]`. Write the package name `iter`.
- For `iter.Seq2`, write `yield k, v`.
- `yield* seq` gives each value of another iterator of the same kind.
- `return` with no value ends the generator.
- If the loop stops early, the generator stops at its current `yield`. Then the `finally` blocks and deferred calls in the generator run.
- Methods and function literals can be generators: `func* (t *Tree) All() iter.Seq[int]`.
- Put each `yield` statement directly in the generator body, not in a function literal.
- `yield *p` gives the value that `p` points to. `yield* p` gives the values of the iterator `p`.

## Collections and strings

### Slice and array methods

Slices and arrays have these methods:

| Method                                   | Result                                                           |
| ---------------------------------------- | ---------------------------------------------------------------- |
| `xs.map(f)`                              | a new slice with `f(x)` for each element                         |
| `xs.filter(f)`                           | a new slice with the elements where `f(x)` is true               |
| `xs.reduce(f)`, `xs.reduce(f, initial)`  | the result of `f(acc, x)` for each element                       |
| `xs.forEach(f)`                          | no result. A statement only.                                     |
| `xs.find(f)`, `xs.findLast(f)`           | the first or last match. See the rules below.                    |
| `xs.findIndex(f)`, `xs.findLastIndex(f)` | the index of the first or last match, or `-1`                    |
| `xs.some(f)`, `xs.every(f)`              | a `bool`                                                         |
| `xs.includes(v)`, `xs.indexOf(v)`        | a `bool`, or the index of `v` or `-1`                            |
| `xs.flatMap(f)`, `xs.flat()`             | a flat slice, one level                                          |
| `xs.sort(cmp)`, `xs.reverse()`           | changes the slice in place and returns it                        |
| `xs.toSorted(cmp)`, `xs.toReversed()`    | a changed copy                                                   |
| `xs.slice(start, end)`                   | a copy. `end` is optional. A negative index counts from the end. |
| `xs.at(i)`                               | the element, or nil. A negative index counts from the end.       |
| `xs.join(sep)`                           | a `string`                                                       |
| `xs.keys()`, `xs.entries()`              | the indexes, or a `[]struct{ Index int; Value T }`               |

```go
const evens = nums.filter(n => n%2 == 0).map(n => n * n)
const labels = nums.map((n, i) => strconv.Itoa(i) + ":" + strconv.Itoa(n))
const total = nums.reduce((acc, n) => acc + float64(n), 0.0)
const name = users.find(u => u.ID == id)?.Name ?? "unknown"
users.sort((a, b) => a.Age - b.Age)
```

Rules:

- A callback can be an arrow function or a function value.
- If the callback has a second parameter, the parameter gets the index.
- Without an initial value, `reduce` starts with the first element. On an empty slice, it panics.
- With an initial value, the accumulator has the type of that value.
- `find`, `findLast`, and `at` return the element if the element type can be nil. Otherwise they return a pointer into the slice, or nil.
- The comparator of `sort` and `toSorted` returns an `int`. The sort is stable. Without a comparator, the elements must be numbers or strings.
- `slice` clamps an index that is out of range.
- `filter` keeps a named slice type. On an array, the result of `filter` is a slice.
- If the type has a method with the same name, the method of the type has priority.
- A chain of `filter` and `map` calls becomes one loop. The callbacks run element by element.

### Map methods

```go
ages.has("ada")                                     // true if the key exists
ages.get("ada") ?? 0                                // the value, or 0 if the key is missing
ages.keys()                                         // []string
ages.values()                                       // []int
ages.entries()                                      // []struct{ Key string; Value int }
ages.delete("ada")                                  // delete(ages, "ada")
ages.forEach((age, name) => fmt.Println(name, age)) // the value comes first, then the key
```

- `get` returns the value if the value type can be nil. Otherwise it returns a pointer to a copy of the value, or nil. A write through this pointer does not change the map.
- The order of `keys`, `values`, `entries`, and `forEach` is random. To sort the keys, assign them to a variable and call `slices.Sort`.
- `delete` and `forEach` can only be statements.
- If the map type has a method with the same name, the method of the type has priority.

### String methods

| Method                   | Output                                       |
| ------------------------ | -------------------------------------------- |
| `s.includes(x)`          | `strings.Contains(s, x)`                     |
| `s.startsWith(x)`        | `strings.HasPrefix(s, x)`                    |
| `s.endsWith(x)`          | `strings.HasSuffix(s, x)`                    |
| `s.split(sep)`           | `strings.Split(s, sep)`                      |
| `s.trim()`               | `strings.TrimSpace(s)`                       |
| `s.trimStart()`          | `strings.TrimLeftFunc(s, unicode.IsSpace)`   |
| `s.trimEnd()`            | `strings.TrimRightFunc(s, unicode.IsSpace)`  |
| `s.toUpperCase()`        | `strings.ToUpper(s)`                         |
| `s.toLowerCase()`        | `strings.ToLower(s)`                         |
| `s.replace(old, new)`    | `strings.Replace(s, old, new, 1)`            |
| `s.replaceAll(old, new)` | `strings.ReplaceAll(s, old, new)`            |
| `s.indexOf(x)`           | `strings.Index(s, x)`                        |
| `s.lastIndexOf(x)`       | `strings.LastIndex(s, x)`                    |
| `s.repeat(n)`            | `strings.Repeat(s, n)`                       |
| `s.slice(start, end)`    | the text from `start` to `end`               |
| `s.padStart(width, pad)` | `pad` before `s` until `s` has `width` bytes |
| `s.padEnd(width, pad)`   | `pad` after `s` until `s` has `width` bytes  |
| `s.at(i)`                | the byte at `i` as a string                  |

- The methods apply to all string types, also named types. The result is a `string`.
- Lengths and indexes count UTF-8 bytes, as in Go. JavaScript counts UTF-16 code units. So the results are different for text that is not ASCII.
- `s.split("")` splits into UTF-8 characters.
- `indexOf` and `lastIndexOf` have no start index. `replace` takes a string, not a regular expression.
- A negative index in `slice` and `at` counts from the end.
- The `pad` argument of `padStart` and `padEnd` is optional. The default is a space.
- `s.at(i)` panics if `i` is out of range.
- If the type has a method with the same name, the method of the type has priority.

### length

`x.length` becomes `len(x)` for slices, arrays, pointers to arrays, strings, maps, and channels.

- For a string, `length` is the number of bytes.
- A field or method with the name `length` has priority.
- Code cannot assign to `length`.

### Struct shorthand and spread

```go
const u = User{name, age}            // User{name: name, age: age}
const all = [...a, x, ...b]          // slices.Concat(a, []int{x}, b)
const p2 = Point{...p, X: 5}         // a copy of p with X set to 5
const p3 = &Point{...*ptr, Y: 1}     // a pointer to a new value
const m = Counts{...a, "n": 1, ...b} // a new map
const q = {...p, X: 5}               // the type comes from the first spread
sum(1, ...xs)                        // sum(1, xs...)
sum(0, ...xs, 2, ...ys)              // sum(0, slices.Concat(xs, []int{2}, ys)...)
```

Struct shorthand:

- gosling uses the shorthand only if a positional literal is not valid. So `Point{y, x}` with two `int` fields sets `X` to `y`, as in Go.

Slice spread:

- A slice spread always makes a new slice.
- Untyped constants get the element type of the spread slices.
- Write `[...a]`. Do not write `[]int{...a}`.

Struct spread:

- The spread must be the first element.
- Name each field after the spread. Shorthand names are valid: `User{...u, age}`.
- For a pointer, write `...*p`.

Map spread:

- A map spread always makes a new map.
- Spreads and entries can come in any order. Later entries win.
- The type before `{` can be a name or a map type.

Call spread:

- A spread fills the variadic parameter. All arguments from the first spread to the end go into it.
- One spread at the end of a call passes the slice itself. So the function can change its elements. Other call spreads make a new slice.

## Type declarations

### Union types

```go
type Shape = Circle | Square | *Polygon
```

Output:

```go
type Shape interface{ isShape() }

func (Circle) isShape()   {}
func (Square) isShape()   {}
func (*Polygon) isShape() {}
```

A type switch on a union in a `.gol` file must list all variants or have a `default` case:

```go
switch const s = s.(type) {
case Circle:
case Square:
}
// error: switch on Shape is missing cases: *Polygon
```

- Declare a union at package level. A union can be in a `type ( ... )` group.
- Each variant must be a type that the same package declares. A variant cannot be an interface.
- `*Polygon` and `Polygon` are different variants.
- If the union lists `Circle`, a `*Circle` value can also be stored. But only `case Circle` counts in the switch check.
- `case nil` is valid. It does not count as a variant.
- gosling does not check switches in `.go` files.

Generic unions:

```go
type Result[T any] = Ok[T] | Err[T]
```

- Each variant must have the type parameters of the union as its type arguments, in the same order.
- `Ok[string]` is not a `Result[int]`.
- A variant without the type parameters is an error. Add the type parameter to the variant type.

### Literal unions

A union of string literals or integer literals becomes a named type with one constant for each literal.

```go
type Method = "GET" | "POST" | "not-found"
// type Method string
// const ( MethodGET Method = "GET"; MethodPOST Method = "POST"; MethodNotFound Method = "not-found" )
// func (m Method) Valid() bool
```

- All literals must be strings, or all literals must be integers.
- A constant name is the union name and the literal. A negative integer, for example `-1`, gives `Neg1`.
- A constant of the union type must be one of the literals. So `let m: Method = "PUT"` is an error.
- A `switch` on the union must list all values or have a `default` case.
- A conversion of a variable, for example `Method(s)`, can make a value that is not in the union. Use `Valid` to check the value.
- A literal union cannot be generic. It needs its own `type` keyword.

### Enums

```go
enum Color { Red, Green, Blue }
// type Color int
// const ( ColorRed Color = iota; ColorGreen; ColorBlue )
// func (v Color) String() string

enum Status { Active = 1, Disabled = 4, Deleted }  // Deleted is 5
enum Level { Debug = "debug", Info = "info" }      // a string enum
```

- Each member becomes a constant. Its name is the enum name, then the member name with an upper case first letter.
- `Color.Red` is the same as `ColorRed`.
- Commas or line breaks separate the members.
- A member without a value is one more than the member before it. The first member is 0.
- `String()` returns the member name. Do not declare a `String` method for an enum.
- A string enum has the type `string`. It has no `String` method.
- Declare enums at package level.
- A `switch` on an enum must list all members or have a `default` case. gosling does not check a switch if a case is not a constant.
- In the value of a member, refer to other members as `Color.Red` or `ColorRed`, not `Red`.

### Optional fields

In a struct type, `Name? T` declares a field of type `*T`.

```go
type Config struct {
	Host? string `json:"host,omitempty"` // Host *string `json:"host,omitempty"`
	Port? int                             // Port *int
}

const cfg = Config{Host: host, Port: 8080}
const port = cfg.Port ?? 80
```

- In a composite literal, an optional field also takes a value of type `T`. gosling stores a pointer to a copy of the value.
- A value of type `*T`, for example `&port` or `nil`, stays as it is.
- Write `Host? string`. Do not write `Host?: string`.
- Declare each optional field on its own line.
- The type of an optional field cannot be a pointer.
- Only composite literals in the same package take a `T` value. An assignment needs a pointer: `cfg.Host = &host`.

### impl blocks

An `impl` block groups the methods of a type. The fields stay in the `type` declaration.

```go
type User struct{ Name string }

impl User implements fmt.Stringer {
	constructor(name: string) {
		this.Name = name
	}

	func String(): string { return this.Name }
}
```

- Each method gets the pointer receiver `this`. For a value receiver, write a Go method outside the block.
- `constructor` becomes `NewUser`, or `newUser` for an unexported type `user`.
- A constructor without result types makes `this` and returns it. Do not use `return` in it.
- A constructor with result types, for example `(*Config, error)`, is a plain function. It has no `this`.
- `implements A, B` adds a compile-time check for each interface.
- A block can contain only methods and one constructor. Declare it at package level.
- `impl Stack[T]` works for generic types. A generic block cannot have a constructor or `implements`.
- There is no `extends`. Embed a type in the struct, as in Go.

## Generated names

- Error variables in the output are `err`, `err2`, and so on.
- Labels in the output are `catch_N`, `end_N`, `finally_N`, and `panic_N`.
- Regex variables are `re1`, `re2`, and so on.
- gosling does not use a name that the file or the package already uses.
- If an import that the output needs has a name that the file already uses, gosling reports an error. Rename your identifier.
- If another file of the package declares the name, the import gets an alias, for example `fmtpkg`.
- If a local declaration hides a type name or a builtin that the output needs, gosling reports an error. Rename the local declaration.

## Line numbers

gosling reports errors with the line and column in the `.gol` file. By default, the output has `/*line file.gol:N*/` comments. So compiler errors, `go vet` results, and stack traces show `.gol` lines.

## Checklist for AI agents

Before you write or change a `.gol` file, read these items:

1. Use `let` and `const`. Do not use `:=` or `var` in a function.
2. In a try block, do not write `if err != nil` after a checked call. gosling adds the check.
3. Outside a try block, receive each error and check it, as in Go.
4. Put `} catch` and `} finally` on the same line.
5. To ignore an error in a try block, write `_ = f()`.
6. In a try block, do not put a checked call in a [position that causes an error](#calls-in-expressions). Move the call to its own statement.
7. Use `throw` to send a new error to the catch block.
8. Use one parameter order in each function: Go order or colon order.
9. Write parameter types on an arrow function that has no context.
10. Do not start a line with the `?` or the `:` of a ternary.
11. In a switch on a union, a literal union, or an enum, list all cases or add `default`.
12. Do not use `defer` or `using` directly in a try block that has `catch panic`.
13. Remember that string lengths and indexes count bytes, not characters.
14. Do not edit the output. Run `gosling ./...`, then `go vet ./...` and `go build ./...`.
15. Run `gosling fmt -w` on each `.gol` file that you change.
