# Known issues and limitations

This file tracks the open bugs, the behavior traps, and the limitations of gosling. The text uses Simplified Technical English (ASD-STE100), as [syntax.md](syntax.md) does.

- When you find an issue, add it here with a short example and a workaround.
- When you fix an issue, remove it here and add a test for it.

The version in this file is gosling v1.0.0.

## Bugs

Each bug has an example that fails or gives incorrect output, and a workaround.

### A ternary with a `nil` arm and no context type gives a typed nil

If one arm is `nil` and the code around the ternary does not give a type, the `nil` gets the type of the other arm. This occurs with `:=`, with `let` without a type, and in an argument to a generic function. When the value later goes into an interface, the interface holds a typed nil, so a nil check is false.

```go
s := c ? &T{} : nil
var x fmt.Stringer = s
// when c is false: x == nil is false
```

A declared type, an assignment, a `return`, or a parameter type of a function that is not generic gives the context type. Then the result is correct.

Workaround: give the declaration a type.

```go
let s: fmt.Stringer = c ? &T{} : nil
```

### An `errors` import is unused with `instanceof`

`err instanceof *fs.PathError` becomes a call to `errors.AsType`. If you import `errors` yourself, gosling reports `"errors" imported and not used`.

```go
import (
	"errors"
	"io/fs"
)

func f(err: error): bool {
	if (err instanceof *fs.PathError) {
		return err.Path != ""
	}
	return false
}
// error: "errors" imported and not used
```

Workaround: do not import `errors` yourself. gosling adds the import.

## Traps

These items work as designed, but the result can be a surprise.

### `return f()` in a try block is not a checked call

gosling does not change a call that already receives its error. A `return` statement receives the error of `f()`, so the error goes to the caller. No catch clause sees it, and a typed catch does not map it.

```go
try {
	return f()               // the error of f goes to the caller
} catch (e is sql.ErrNoRows) {
	return 0, ErrNotFound    // does not run
}
```

If a catch clause must see the error, declare the result first:

```go
try {
	const n = f()
	return n, nil
} catch (e is sql.ErrNoRows) {
	return 0, ErrNotFound
}
```

### A spread or a slice method can return nil

`[...a]` becomes `slices.Concat`. For empty input, `slices.Concat` returns nil. `filter` and `map` can also return nil for empty input. `encoding/json` encodes a nil slice as `null`, not `[]`.

If the code needs an empty slice that is not nil, make it with `make` or with a composite literal.

### gosling does not remove imports

gosling adds the imports that the output needs. It does not remove an import that becomes unused, for example `strings` after you change `strings.TrimSpace(s)` to `s.trim()`. Go then reports `"strings" imported and not used`. Remove the import yourself.

### `gosling fmt -l` exits 0 when it lists files

`gosling fmt -l` does the same as `gofmt -l`. It lists the files that are not formatted, and its exit status is 0. To fail a check, test the output:

```sh
test -z "$(gosling fmt -l ./...)"
```

### A value ternary becomes a function literal

The output of a ternary that is a value is a function literal that the code calls immediately. The behavior is correct, but the output is longer than an `if` statement. A ternary that is a statement becomes an `if` statement.

## Limitations

These items are not supported, or are not supported yet.

### Errors and try

- There is no `?` operator for errors, for example `f()?`. Use try and catch.
- Without `catch panic`, a panic does not run `finally` blocks. A panic in a catch block or in a catch panic block also does not run them.
- `catch panic` cannot follow a typed catch clause.
- In a try block, an optional call that returns an error must be a statement to be checked.
- Checks do not apply in a function literal or an arrow function in a try block. Put a new try block in the function.

### Declarations

- `using` declares one name. It cannot have a type or a destructuring pattern.
- `using` in a loop is an error, because `Close` runs when the function returns.
- Default values are not supported in generic functions, variadic functions, function literals, arrow functions, and interface methods.
- A default value must be plain Go. It cannot use gosling syntax, such as a template literal.
- Only composite literals in the same package take a `T` value for an optional field. Other packages see a plain `*T` field. An assignment also needs a pointer: `cfg.Host = &host`.

### Expressions

- A line cannot start with the `?` or the `:` of a ternary.
- A skipped `?.` chain gives the zero value. Code cannot tell a nil link from a real zero value, unless the last link is a pointer or the chain ends in a comma-ok type assertion or map index.
- `?.[` cannot start a slice expression. `xs?.[1:]` is an error.
- Regex literals use RE2 syntax. JavaScript-only syntax, such as lookahead `(?=...)` and backreferences `\1`, is not available.
- A tagged template cannot have a format, such as `${x:.2f}`.

### Destructuring

- Object patterns have no rest element.
- A channel receive has no pattern form. `const [msg, open] = <-ch` is an error.

### Functions

- An arrow function without a context needs parameter types. gosling does not use the body to find parameter types.
- An arrow cannot get a type from the context if a local declaration hides the name of that type. Rename the local declaration.
- Only a package-level `const` with one name can hold a generic arrow. Go function literals cannot have type parameters.
- In a `case` expression, a function type or a function literal cannot use `):`. Use Go order there.
- A `yield` statement must be directly in a generator body, not in a function literal in it. Arrow functions cannot be generators.

### Types

- Union variants must be in the same package and cannot be interfaces.
- gosling checks exhaustiveness only for switches in `.gol` files. It does not check `if` chains or switches in `.go` files.
- In the value of an enum member, refer to other members as `Color.Red` or `ColorRed`, not `Red`.

### Editors

- VS Code highlights the name in `name: type` parameters as a type.

### Language server

- Code that gosling generates, such as `try` and `catch`, has no definition.
- A definition in generated code, such as a catch variable, goes to the nearest token before it.
- A rename can change a name that gosling copies into generated code, such as a catch variable, an arrow parameter, or a destructured name. The server then transpiles the renamed files and compares the result with the rename in Go. If they differ, the rename fails, because a partial rename would break the code. For example, a rename of the field `X` fails if `const {X} = p` uses it.
- A rename in a file that changed after a syntax error fails if the name occurs in generated code.
- After a syntax error, the server uses the last version that parsed, with the changed lines that still parse. The lines that do not parse get no results until the file parses again. Type errors do not have this problem.
- gopls uses only the build configuration of your system. A `.gol` file for another system or with a build tag gets no results from gopls.
- A default value has no hover or definition. The outline also shows its helper function.
- A rename of `this` or of a constructor fails. gosling generates their declarations.
