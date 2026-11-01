package testsupport

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The permission contract (#144), read from the source of every package
// under internal/.
//
// Every exported function that takes an access.Scope says who may call it
// and in which module, in the code itself:
//
//   - a role check, recognised in its own body: scope.Role.<method>,
//     a call passing scope.Role, or scope.PlansStaff(). Seen at the operation
//     because which role may do a thing is decided per operation; or it is
//     in the package's everyRole map with the reason any role may.
//
//   - a module check, recognised in its own body or in any function of the
//     same package it calls, however deep: scope.Require(access.<Module>),
//     scope.Uses(access.<Module>), or a package helper built on them such as
//     the workshop's inWorkshop. Followed into calls because the check
//     belongs where the rows are read, which is usually a helper; or it is
//     in the package's core map with the reason it belongs to no module.
//
// Both maps are package-level map[string]string literals in the package's
// own _test.go files, named everyRole and core. A package that takes scopes
// and declares neither fails: a new module cannot opt out by omission. An
// empty map is a declaration: "nothing here is open to every role".
//
// Skipped by rule, never by name: _test.go files, files that say they are
// generated ("Code generated ... DO NOT EDIT."), testdata directories, and
// methods -- an operation is a function, the way every package here writes
// them.
//
// A function whose only job is to pass the scope on still counts: it checks,
// or it is listed with the reason (usually "delegates to X, which checks").
// An operation run by a background job with no person behind it says in its
// reason how the job forms its scope.

// ContractMaps are the names of the declarations the contract reads.
const (
	RoleMapName   = "everyRole"
	ModuleMapName = "core"
)

var (
	roleCheck   = regexp.MustCompile(`scope\.Role\.|\(scope\.Role,|scope\.PlansStaff\(\)`)
	moduleCheck = regexp.MustCompile(`scope\.(Require|Uses)\(access\.`)
	generated   = regexp.MustCompile(`(?m)^// Code generated .* DO NOT EDIT\.$`)
)

// ContractReport is what one walk found.
type ContractReport struct {
	// Problems, each naming the function and its file, or the package.
	Problems []string
	// Operations checked, per package directory relative to the root.
	Checked map[string]int
}

// CheckContracts walks every package under root but those in skip, and
// reports every exported function taking an access.Scope that says neither
// who may call it nor which module it belongs to, every map entry that
// contradicts the code, and every package that takes scopes without the
// maps.
func CheckContracts(root string, skip map[string]string) (ContractReport, error) {
	report := ContractReport{Checked: map[string]int{}}
	var dirs []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".") && path != root {
			return filepath.SkipDir
		}
		if _, skipped := skip[filepath.ToSlash(rel)]; skipped {
			return filepath.SkipDir
		}
		dirs = append(dirs, path)
		return nil
	})
	if err != nil {
		return report, err
	}
	for _, dir := range dirs {
		rel, _ := filepath.Rel(root, dir)
		rel = filepath.ToSlash(rel)
		problems, n, err := checkPackage(dir, rel)
		if err != nil {
			return report, err
		}
		report.Problems = append(report.Problems, problems...)
		if n > 0 {
			report.Checked[rel] = n
		}
	}
	return report, nil
}

type operation struct {
	name, file string
}

func checkPackage(dir, rel string) (problems []string, checked int, err error) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, err
	}
	bodies := map[string]string{}
	var ops []operation
	var maps = map[string]map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		path := filepath.Join(dir, name)
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, 0, err
		}
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return nil, 0, fmt.Errorf("parse %s: %w", path, err)
		}
		if strings.HasSuffix(name, "_test.go") {
			for _, m := range []string{RoleMapName, ModuleMapName} {
				if lit, ok := mapLiteral(file, m); ok {
					if maps[m] == nil {
						maps[m] = map[string]string{}
					}
					for k, v := range lit {
						maps[m][k] = v
					}
				}
			}
			continue
		}
		if generated.Match(src) {
			continue
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Body == nil {
				continue
			}
			bodies[fn.Name.Name] = string(src[fset.Position(fn.Body.Pos()).Offset:fset.Position(fn.Body.End()).Offset])
			if fn.Name.IsExported() && takesScope(fn) {
				ops = append(ops, operation{fn.Name.Name, filepath.Join(rel, name)})
			}
		}
	}
	if len(ops) == 0 {
		return nil, 0, nil
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].name < ops[j].name })
	for _, m := range []string{RoleMapName, ModuleMapName} {
		if maps[m] == nil {
			problems = append(problems, fmt.Sprintf("%s: takes scopes and declares no %s map in its tests; "+
				"declare one, empty if nothing belongs in it", rel, m))
		}
	}
	modules := moduleChecker(bodies)
	for _, op := range ops {
		body := bodies[op.name]
		_, open := maps[RoleMapName][op.name]
		_, isCore := maps[ModuleMapName][op.name]
		role := roleCheck.MatchString(body)
		module := modules(op.name)
		switch {
		case !role && !open:
			problems = append(problems, fmt.Sprintf("%s (%s) takes a scope and checks no role; check it, "+
				"or add it to %s with the reason any role may call it", op.name, op.file, RoleMapName))
		case role && open:
			problems = append(problems, fmt.Sprintf("%s (%s) checks a role and is also listed in %s; one of the two is wrong",
				op.name, op.file, RoleMapName))
		}
		switch {
		case !module && !isCore:
			problems = append(problems, fmt.Sprintf("%s (%s) takes a scope and checks no module; check it, "+
				"or add it to %s with the reason it belongs to no module", op.name, op.file, ModuleMapName))
		case module && isCore:
			problems = append(problems, fmt.Sprintf("%s (%s) checks a module and is also listed in %s; "+
				"a Core operation must keep working whatever the shop runs", op.name, op.file, ModuleMapName))
		}
	}
	for _, m := range []string{RoleMapName, ModuleMapName} {
		for name := range maps[m] {
			if !hasOp(ops, name) {
				problems = append(problems, fmt.Sprintf("%s: %s lists %s, which is not an operation of the package any more",
					rel, m, name))
			}
		}
	}
	return problems, len(ops), nil
}

func hasOp(ops []operation, name string) bool {
	for _, o := range ops {
		if o.name == name {
			return true
		}
	}
	return false
}

func takesScope(fn *ast.FuncDecl) bool {
	for _, p := range fn.Type.Params.List {
		if se, ok := p.Type.(*ast.SelectorExpr); ok && se.Sel.Name == "Scope" {
			if x, ok := se.X.(*ast.Ident); ok && x.Name == "access" {
				return true
			}
		}
	}
	return false
}

// mapLiteral reads a package-level map[string]string literal of string
// literals.
func mapLiteral(file *ast.File, name string) (map[string]string, bool) {
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, n := range vs.Names {
				if n.Name != name || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.CompositeLit)
				if !ok {
					continue
				}
				out := map[string]string{}
				for _, elt := range lit.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					k, kok := kv.Key.(*ast.BasicLit)
					v, vok := kv.Value.(*ast.BasicLit)
					if !kok || !vok {
						continue
					}
					ks, _ := strconv.Unquote(k.Value)
					vsv, _ := strconv.Unquote(v.Value)
					out[ks] = vsv
				}
				return out, true
			}
		}
	}
	return nil, false
}

// moduleChecker answers whether a function checks a module, in its own body
// or in a function of the package it calls.
func moduleChecker(bodies map[string]string) func(string) bool {
	memo := map[string]bool{}
	visiting := map[string]bool{}
	calls := map[string]*regexp.Regexp{}
	for name := range bodies {
		calls[name] = regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\(`)
	}
	var checks func(string) bool
	checks = func(name string) bool {
		if v, ok := memo[name]; ok {
			return v
		}
		if visiting[name] {
			return false
		}
		visiting[name] = true
		body := bodies[name]
		found := moduleCheck.MatchString(body)
		for callee, re := range calls {
			if found {
				break
			}
			if callee != name && strings.Contains(body, callee+"(") && re.MatchString(body) && checks(callee) {
				found = true
			}
		}
		visiting[name] = false
		memo[name] = found
		return found
	}
	return checks
}
