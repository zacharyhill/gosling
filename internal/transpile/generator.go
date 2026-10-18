package transpile

import (
	"go/ast"
)

// reportNestedYields reports each yield statement in a function literal
// inside a generator. Its return would end the literal, not the generator.
func (transpiler *transpiler) reportNestedYields(file *file) {
	if len(file.preprocessed.Yields) == 0 {
		return
	}
	parentOf, yieldAt, generatorAt := parents(file.ast), offsetSet(file.preprocessed.Yields), offsetSet(file.preprocessed.Generators)
	ast.Inspect(file.ast, func(node ast.Node) bool {
		stmt, ok := node.(ast.Stmt)
		if !ok || !yieldAt[file.tokenFile.Offset(stmt.Pos())] {
			return true
		}
		literal, ok := enclosingFunc(parentOf, stmt).(*ast.FuncLit)
		if !ok || !generatorAt[file.tokenFile.Offset(literal.Pos())] {
			transpiler.errorf(file, stmt.Pos(), "yield must be directly in a generator body, not in a nested function")
		}
		return true
	})
}

func offsetSet(offsets []int) map[int]bool {
	set := make(map[int]bool, len(offsets))
	for _, offset := range offsets {
		set[offset] = true
	}
	return set
}
