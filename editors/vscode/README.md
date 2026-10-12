# gosling for VS Code

Syntax highlighting for gosling `.gol` files.

The extension uses the Go grammar that ships with VS Code, then adds the gosling syntax: `try` / `catch` / `finally`, `let`, `for...of`, template literals, arrow functions, `?.`, `??`, and union types.

The extension gives a separate `gol` language ID to `.gol` files. Thus the Go extension and gopls do not open them. The extension does syntax highlighting only. Go to Definition (Ctrl+click), hover, and completion need a language server. A gosling language server is in progress.

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
