import assert from "node:assert/strict";
import { test } from "node:test";
import { tokenize } from "./tokenize.mjs";

function scopesOf(source, text, occurrence = 0) {
  const matches = tokenize(source).filter((token) => token.text.trim() === text);
  assert.ok(matches.length > occurrence, `no token "${text}" (occurrence ${occurrence}) in: ${source}`);
  return matches[occurrence].scopes;
}

function hasScopePrefix(scopes, prefix) {
  return scopes.some((scope) => scope.startsWith(prefix));
}

function assertScope(source, text, wantScope, occurrence = 0) {
  const scopes = scopesOf(source, text, occurrence);
  assert.ok(
    hasScopePrefix(scopes, wantScope),
    `"${text}" in ${JSON.stringify(source)} has ${JSON.stringify(scopes)}, want ${wantScope}`,
  );
}

function assertNoScope(source, text, unwantedScope, occurrence = 0) {
  const scopes = scopesOf(source, text, occurrence);
  assert.ok(
    !hasScopePrefix(scopes, unwantedScope),
    `"${text}" in ${JSON.stringify(source)} has ${JSON.stringify(scopes)}, want no ${unwantedScope}`,
  );
}

function inFunctionBody(body) {
  return `package p\n\nfunc f() {\n${body}\n}`;
}

test("plain Go still highlights", () => {
  assertScope(inFunctionBody("\treturn"), "return", "keyword.control.go");
  assertScope('package p\n\nvar s = "hi"', '"', "string.quoted.double.go");
});

test("try, catch, finally are keywords", () => {
  const source = inFunctionBody("\ttry {\n\t\tg()\n\t} catch (e) {\n\t\th(e)\n\t} finally {\n\t\tdone()\n\t}");
  assertScope(source, "try", "keyword.control.exception.gol");
  assertScope(source, "catch", "keyword.control.exception.gol");
  assertScope(source, "finally", "keyword.control.exception.gol");
  assertScope(source, "e", "variable.parameter.gol");
});

test("throw is a keyword", () => {
  assertScope(inFunctionBody("\tthrow fmt.Errorf(\"x\")"), "throw", "keyword.control.exception.gol");
  assertNoScope(inFunctionBody("\tthrows(err)"), "throws", "keyword");
});

test("typed catch clauses", () => {
  const source = inFunctionBody("\t} catch (pathError: *fs.PathError) {\n\t} catch (missing is sql.ErrNoRows) {");
  assertScope(source, "pathError", "variable.parameter.gol");
  assertScope(source, ":", "punctuation.separator.type.gol");
  assertScope(source, "missing", "variable.parameter.gol");
  assertScope(source, "is", "keyword.operator.is.gol");
  assertScope(source, "catch", "keyword.control.exception.gol", 1);
});

test("catch without a variable", () => {
  assertScope(inFunctionBody("\t} catch {"), "catch", "keyword.control.exception.gol");
});

test("catch panic is a keyword pair", () => {
  const source = inFunctionBody("\t} catch panic (r) {\n\t\th(r)\n\t}");
  assertScope(source, "catch", "keyword.control.exception.gol");
  assertScope(source, "panic", "keyword.control.exception.gol");
  assertScope(source, "r", "variable.parameter.gol");
  assertScope(inFunctionBody("\t} catch panic {"), "panic", "keyword.control.exception.gol");
});

test("keywords inside strings and comments stay plain", () => {
  assertNoScope(inFunctionBody('\ts := "try catch"'), "try catch", "keyword");
  assertNoScope(inFunctionBody("\t// let try"), "let try", "keyword");
  assertNoScope(inFunctionBody("\ts := `let`"), "let", "keyword");
});

test("let is a declaration keyword", () => {
  assertScope(inFunctionBody("\tlet n = 0"), "let", "keyword.let.gol");
  assertScope(inFunctionBody("\tlet n = 0"), "n", "variable.other.declaration.gol");
});

test("let with type annotation", () => {
  const source = inFunctionBody("\tlet cfg: Config");
  assertScope(source, "let", "keyword.let.gol");
  assertScope(source, "cfg", "variable.other.declaration.gol");
  assertScope(source, "Config", "entity.name.type.gol");
  assertScope(inFunctionBody("\tlet names: []string"), "string", "storage.type");
  assertScope(inFunctionBody("\tlet x, y: int"), "y", "variable.other.declaration.gol");
});

test("const in function bodies", () => {
  assertScope(inFunctionBody("\tconst data = os.ReadFile(path)"), "const", "keyword.const");
});

test("identifiers containing let are not keywords", () => {
  assertNoScope(inFunctionBody("\tlettuce := 1"), "lettuce", "keyword");
  assertNoScope(inFunctionBody("\ttryAgain()"), "tryAgain", "keyword");
});

test("for of and for in", () => {
  const forOf = inFunctionBody("\tfor (const x of xs) {\n\t}");
  assertScope(forOf, "for", "keyword.control");
  assertScope(forOf, "const", "keyword.const");
  assertScope(forOf, "x", "variable.other.declaration.gol");
  assertScope(forOf, "of", "keyword.operator.of.gol");
  assertScope(inFunctionBody("\tfor (let k in m) {\n\t}"), "in", "keyword.operator.in.gol");
});

test("C-style for with let", () => {
  const source = inFunctionBody("\tfor (let i = 0; i < n; i++) {\n\t}");
  assertScope(source, "let", "keyword.let.gol");
  assertScope(source, "i", "variable.other.declaration.gol");
});

test("template literals", () => {
  const source = inFunctionBody("\ts := t`hello ${user.Name}, ${n ?? 0}`");
  assertScope(source, "t", "storage.type.template.gol");
  assertScope(source, "hello", "string.template.gol");
  assertScope(source, "${", "punctuation.definition.template-expression.begin.gol");
  assertScope(source, "Name", "meta.template.expression.gol");
  assertScope(source, "??", "keyword.operator.nullish.gol");
  assertScope(source, "}", "punctuation.definition.template-expression.end.gol", 1);
  assertScope(source, "`", "punctuation.definition.string.template.end.gol", 1);
});

test("tagged template literals", () => {
  const source = inFunctionBody("\tq := db.SQL`select ${id}`");
  assertScope(source, "SQL", "entity.name.function.tag.gol");
  assertScope(source, "select", "string.template.gol");
  assertScope(source, "id", "meta.template.expression.gol");
});

test("template literal escapes", () => {
  assertScope(inFunctionBody("\ts := t`a\\tb \\${x}`"), "\\t", "constant.character.escape.gol");
  assertScope(inFunctionBody("\ts := t`a\\tb \\${x}`"), "\\$", "constant.character.escape.gol");
});

test("escaped backtick in a template literal", () => {
  const source = inFunctionBody("\ts := t`a\\`b`");
  assertScope(source, "\\`", "constant.character.escape.gol");
  assertScope(source, "b", "string.template.gol");
});

test("multiline template literal", () => {
  const source = inFunctionBody("\tlet msg = t`Dear ${name},\nthanks!`");
  assertScope(source, "thanks!", "string.template.gol");
});

test("template literal holding a nested template", () => {
  const source = inFunctionBody("\ts := t`a ${t`b ${c}`} d`");
  assertScope(source, "d", "string.template.gol");
  assertScope(source, "c", "meta.template.expression.gol");
});

test("raw strings are not template literals", () => {
  const source = inFunctionBody("\ts := `${x}`");
  assertScope(source, "${x}", "string.quoted.raw.go");
});

test("arrow functions", () => {
  assertScope(inFunctionBody("\tg := xs.map(x => x * 2)"), "=>", "storage.type.function.arrow.gol");
  assertScope(inFunctionBody("\tg := xs.map(x => x * 2)"), "x", "variable.parameter.gol");
  assertScope(inFunctionBody("\tsort.Slice(xs, (i, j) => xs[i] < xs[j])"), "=>", "storage.type.function.arrow.gol");
});

test("optional chaining and nullish coalescing", () => {
  const source = inFunctionBody('\tname := user?.Profile?.Name ?? "anon"');
  assertScope(source, "?.", "punctuation.accessor.optional.gol");
  assertScope(source, "??", "keyword.operator.nullish.gol");
});

test("ternary operator", () => {
  const source = inFunctionBody('\tsign := n < 0 ? "neg" : n?.x ?? "pos"');
  assertScope(source, "?", "keyword.operator.ternary.gol");
  assertScope(source, "?.", "punctuation.accessor.optional.gol");
  assertScope(source, "??", "keyword.operator.nullish.gol");
});

test("union types", () => {
  const source = "package p\n\ntype Shape = Circle | Square | *Polygon";
  assertScope(source, "Shape", "entity.name.type");
  assertScope(source, "Circle", "entity.name.type.gol");
  assertScope(source, "Square", "entity.name.type.gol");
  assertScope(source, "Polygon", "entity.name.type.gol");
  assertScope(source, "|", "punctuation.separator.union.gol");
});

test("switch with let type binding", () => {
  assertScope(inFunctionBody("\tswitch let s = s.(type) {\n\t}"), "let", "keyword.let.gol");
});

test("destructuring declarations", () => {
  assertScope(inFunctionBody("\tconst {Name: name, Port} = cfg"), "const", "keyword.const");
  assertScope(inFunctionBody("\tlet [first, ...rest] = xs"), "let", "keyword.let.gol");
  assertScope(inFunctionBody("\tlet [first, ...rest] = xs"), "...", "keyword.operator");
});
