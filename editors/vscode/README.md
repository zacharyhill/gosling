# gosling for VS Code

Syntax highlighting and a language server for gosling `.gol` files.

The extension uses the Go grammar that ships with VS Code, then adds the gosling syntax: `try` / `catch` / `finally`, `let`, `for...of`, template literals, regex literals, arrow functions, `?.`, `??`, and union types.

The extension gives a separate `gol` language ID to `.gol` files. Thus the Go extension does not open them.

The extension also starts the gosling language server, `gosling lsp`. The server gives Go to Definition (Ctrl+click), references, hover, completion, signature help, rename, the outline, and diagnostics. It needs `gosling` and `gopls` on the PATH. To use other paths, set `gosling.path` and `gosling.goplsPath`.

When you save a `.gol` file, the extension writes the generated `.go` files of its package. When you rename or delete a `.gol` file, the extension deletes its old `.go` file. To stop this, set `gosling.generateOnSave` to `false`. The server restarts when you change a gosling setting.

## Install

Install `gosling` and `gopls` first. Run these commands from the root of the repository:

```sh
go install ./cmd/gosling
go install golang.org/x/tools/gopls@latest
```

Then build and install the extension from this directory:

```sh
npm install
npm run package
code --install-extension gosling.vsix
```

Then run **Developer: Reload Window** in VS Code.

The first requests can be slow. gopls loads and type-checks the workspace when it starts. After that, requests are fast.

If the server does not start, open the Output panel (Ctrl+Shift+U) and select **gosling**. The panel shows the server errors.

## Test

```sh
npm test
```

The tests load the Go grammar from a local VS Code install. If the tests cannot find it, set `GO_GRAMMAR` to the path of `extensions/go/syntaxes/go.tmLanguage.json` in your VS Code install.
