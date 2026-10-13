import { existsSync, readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import vsctm from "vscode-textmate";
import oniguruma from "vscode-oniguruma";

const require = createRequire(import.meta.url);
const extensionDir = join(dirname(fileURLToPath(import.meta.url)), "..");

// The Go grammar ships with VS Code, not npm, so tests load it from a local install.
const goGrammarCandidates = [
  process.env.GO_GRAMMAR,
  "/snap/code/current/usr/share/code/resources/app/extensions/go/syntaxes/go.tmLanguage.json",
  "/usr/share/code/resources/app/extensions/go/syntaxes/go.tmLanguage.json",
  "/Applications/Visual Studio Code.app/Contents/Resources/app/extensions/go/syntaxes/go.tmLanguage.json",
].filter(Boolean);

function findGoGrammar() {
  const goGrammarPath = goGrammarCandidates.find((path) => existsSync(path));
  if (!goGrammarPath) {
    throw new Error("Go grammar not found. Set GO_GRAMMAR to the path of go.tmLanguage.json from VS Code.");
  }
  return goGrammarPath;
}

const grammarPaths = {
  "source.go": findGoGrammar(),
  "source.gol": join(extensionDir, "syntaxes", "gol.tmLanguage.json"),
  "gosling.injection": join(extensionDir, "syntaxes", "gol.injection.tmLanguage.json"),
};

const onigurumaWasm = readFileSync(require.resolve("vscode-oniguruma/release/onig.wasm"));
await oniguruma.loadWASM(onigurumaWasm.buffer);

const registry = new vsctm.Registry({
  onigLib: Promise.resolve({
    createOnigScanner: (patterns) => new oniguruma.OnigScanner(patterns),
    createOnigString: (text) => new oniguruma.OnigString(text),
  }),
  loadGrammar: async (scopeName) => {
    const path = grammarPaths[scopeName];
    if (!path) {
      return null;
    }
    return vsctm.parseRawGrammar(readFileSync(path, "utf8"), path);
  },
  getInjections: (scopeName) => (scopeName === "source.gol" ? ["gosling.injection"] : []),
});

const grammar = await registry.loadGrammar("source.gol");

export function tokenize(source) {
  const tokens = [];
  let ruleStack = vsctm.INITIAL;
  for (const line of source.split("\n")) {
    const lineTokens = grammar.tokenizeLine(line, ruleStack);
    for (const token of lineTokens.tokens) {
      tokens.push({ text: line.slice(token.startIndex, token.endIndex), scopes: token.scopes });
    }
    ruleStack = lineTokens.ruleStack;
  }
  return tokens;
}
