# gosling for VS Code

Syntax highlighting and a language server for gosling `.gol` files.

The extension uses the Go grammar that ships with VS Code, then adds the gosling syntax: `try` / `catch` / `finally`, `let`, `for...of`, template literals, arrow functions, `?.`, `??`, and union types.

The extension gives a separate `gol` language ID to `.gol` files. Thus the Go extension does not open them.

The extension also starts the gosling language server, `gosling lsp`. The server gives Go to Definition (Ctrl+click), references, hover, completion, signature help, the outline, and diagnostics. It needs `gosling` and `gopls` on the PATH. To use other paths, set `gosling.path` and `gosling.goplsPath`. Then run **gosling: Restart Language Server**.

## Install

```sh
npm install
npm run package
code --install-extension gosling-0.1.0.vsix
```

## Test

```sh
npm test
```

The tests load the Go grammar from a local VS Code install. If the tests cannot find it, set `GO_GRAMMAR` to the path of `extensions/go/syntaxes/go.tmLanguage.json` in your VS Code install.
