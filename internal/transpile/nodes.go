package transpile

import (
	"go/ast"
	"go/types"

	"golang.org/x/tools/go/ast/astutil"
)

func replaceNode(root ast.Node, oldNode, replacement ast.Node) {
	astutil.Apply(root, func(cursor *astutil.Cursor) bool {
		if cursor.Node() == oldNode {
			cursor.Replace(replacement)
			return false
		}
		return true
	}, nil)
}

func parents(root ast.Node) map[ast.Node]ast.Node {
	parentOf := map[ast.Node]ast.Node{}
	var stack []ast.Node
	ast.Inspect(root, func(node ast.Node) bool {
		if node == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		if len(stack) > 0 {
			parentOf[node] = stack[len(stack)-1]
		}
		stack = append(stack, node)
		return true
	})
	return parentOf
}

// outermostParen returns node with any wrapping parentheses, and the node
// that contains the result.
func outermostParen(parentOf map[ast.Node]ast.Node, node ast.Node) (ast.Node, ast.Node) {
	parent := parentOf[node]
	for {
		paren, ok := parent.(*ast.ParenExpr)
		if !ok {
			return node, parent
		}
		node, parent = paren, parentOf[paren]
	}
}

func enclosingSig(info *types.Info, parentOf map[ast.Node]ast.Node, node ast.Node) *types.Signature {
	for ancestor := parentOf[node]; ancestor != nil; ancestor = parentOf[ancestor] {
		switch funcNode := ancestor.(type) {
		case *ast.FuncLit:
			signature, _ := info.Types[funcNode].Type.(*types.Signature)
			return signature
		case *ast.FuncDecl:
			if object := info.Defs[funcNode.Name]; object != nil {
				signature, _ := object.Type().(*types.Signature)
				return signature
			}
			return nil
		}
	}
	return nil
}

func enclosingFunc(parentOf map[ast.Node]ast.Node, node ast.Node) ast.Node {
	for ancestor := parentOf[node]; ancestor != nil; ancestor = parentOf[ancestor] {
		switch ancestor.(type) {
		case *ast.FuncLit, *ast.FuncDecl:
			return ancestor
		}
	}
	return nil
}

func callTo(expr ast.Expr, name string) (*ast.CallExpr, bool) {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return nil, false
	}
	ident, ok := call.Fun.(*ast.Ident)
	return call, ok && ident.Name == name
}
