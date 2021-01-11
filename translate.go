package cxgo

import (
	"bytes"
	"fmt"
	"go/format"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gotranspile/cxgo/libs"
	"github.com/gotranspile/cxgo/types"
	"modernc.org/cc/v3"
)

type Config struct {
	Root               string
	Package            string
	GoFile             string
	Include            []string
	SysInclude         []string
	MaxDecls           int
	Predef             string
	Define             []Define
	FlattenAll         bool
	ForwardDecl        bool
	SkipDecl           map[string]bool
	Idents             []IdentConfig
	Replace            []Replacer
	Hooks              bool
	FixImplicitReturns bool
	IgnoreIncludeDir   bool
}

type TypeHint string

const (
	HintBool  = TypeHint("bool")  // force the type to Go bool
	HintSlice = TypeHint("slice") // force type to Go slice (for pointers and arrays)
	HintIface = TypeHint("iface") // force type to Go interface{}
)

type IdentConfig struct {
	Name    string        `yaml:"name" json:"name"`       // identifier name in C
	Rename  string        `yaml:"rename" json:"rename"`   // rename the identifier
	Alias   bool          `yaml:"alias" json:"alias"`     // omit declaration, use underlying type instead
	Type    TypeHint      `yaml:"type" json:"type"`       // changes the Go type of this identifier
	Flatten *bool         `yaml:"flatten" json:"flatten"` // flattens function control flow to workaround invalid gotos
	Fields  []IdentConfig `yaml:"fields" json:"fields"`   // configs for struct fields or func arguments
}

type Replacer struct {
	Old string
	Re  *regexp.Regexp
	New string
}

func Translate(root, fname, out string, env *libs.Env, conf Config) error {
	cname := fname
	tu, err := Parse(env, root, cname, SourceConfig{
		Predef:           conf.Predef,
		Define:           conf.Define,
		Include:          conf.Include,
		SysInclude:       conf.SysInclude,
		IgnoreIncludeDir: conf.IgnoreIncludeDir,
	})
	if err != nil {
		return fmt.Errorf("parsing failed: %w", err)
	}
	decls, err := TranslateAST(cname, tu, env, conf)
	if err != nil {
		return err
	}
	pkg := conf.Package
	if pkg == "" {
		pkg = "lib"
	}
	_ = os.MkdirAll(out, 0755)
	bbuf := bytes.NewBuffer(nil)
	gofile := conf.GoFile
	if gofile == "" {
		gofile, err = filepath.Rel(root, fname)
		if err != nil {
			return err
		}
		// flatten C source file path to make a single large Go package
		// TODO: auto-generate Go packages based on dir structure
		gofile = strings.ReplaceAll(gofile, string(filepath.Separator), "_")
		gofile = strings.TrimSuffix(gofile, ".c")
		gofile = strings.TrimSuffix(gofile, ".h")
		gofile += ".go"
	}
	max := conf.MaxDecls
	if max == 0 {
		max = 100
	}
	// optionally split large files by N declaration per file
	for i := 0; len(decls) > 0; i++ {
		cur := decls
		if max > 0 && len(cur) > max {
			cur = cur[:max]
		}
		decls = decls[len(cur):]

		// generate Go file header with a package name and a list of imports
		header := goHeader(env, cur)
		buf := make([]GoDecl, 0, len(header)+len(cur))
		buf = append(buf, header...)
		buf = append(buf, cur...)

		bbuf.Reset()
		err = PrintGo(bbuf, pkg, buf)
		if err != nil {
			return err
		}
		suff := fmt.Sprintf("_p%d", i+1)
		if i == 0 && len(decls) == 0 {
			suff = ""
		}
		gopath := strings.TrimSuffix(gofile, ".go") + suff + ".go"
		if !filepath.IsAbs(gopath) {
			gopath = filepath.Join(out, gopath)
		}

		fdata := bbuf.Bytes()
		// run replacements defined in the config
		for _, rep := range conf.Replace {
			if rep.Re != nil {
				fdata = rep.Re.ReplaceAll(fdata, []byte(rep.New))
			} else {
				fdata = bytes.ReplaceAll(fdata, []byte(rep.Old), []byte(rep.New))
			}
		}

		fmtdata, err := format.Source(fdata)
		if err != nil {
			// write anyway for examination
			_ = ioutil.WriteFile(gopath, fdata, 0644)
			return fmt.Errorf("error formatting %s: %v", filepath.Base(gofile), err)
		}
		err = ioutil.WriteFile(gopath, fmtdata, 0644)
		if err != nil {
			return err
		}
	}
	return nil
}

// TranslateAST takes a C translation unit and converts it to a list of Go declarations.
func TranslateAST(fname string, tu *cc.AST, env *libs.Env, conf Config) ([]GoDecl, error) {
	t := newTranslator(env, conf)
	return t.translate(fname, tu), nil
}

// TranslateCAST takes a C translation unit and converts it to a list of cxgo declarations.
func TranslateCAST(fname string, tu *cc.AST, env *libs.Env, conf Config) ([]CDecl, error) {
	t := newTranslator(env, conf)
	return t.translateC(fname, tu), nil
}

func newTranslator(env *libs.Env, conf Config) *translator {
	tr := &translator{
		env:       env,
		tenv:      env.Clone(),
		conf:      conf,
		idents:    make(map[string]IdentConfig),
		ctypes:    make(map[cc.Type]types.Type),
		decls:     make(map[cc.Node]*types.Ident),
		namedPtrs: make(map[string]types.PtrType),
		named:     make(map[string]types.Named),
		aliases:   make(map[string]types.Type),
	}
	for _, v := range conf.Idents {
		tr.idents[v.Name] = v
	}
	_, _ = tr.tenv.GetLibrary(libs.BuiltinH)
	_, _ = tr.tenv.GetLibrary(libs.StdlibH)
	_, _ = tr.tenv.GetLibrary(libs.StdioH)
	return tr
}

type translator struct {
	env  *libs.Env
	tenv *libs.Env // virtual env for stdlib forward declarations
	conf Config

	file *cc.AST
	cur  string

	idents    map[string]IdentConfig
	ctypes    map[cc.Type]types.Type
	namedPtrs map[string]types.PtrType
	named     map[string]types.Named
	aliases   map[string]types.Type
	decls     map[cc.Node]*types.Ident
}

func (g *translator) Nil() Nil {
	return NewNil(g.env.PtrSize())
}

func (g *translator) Iota() Expr {
	return IdentExpr{g.env.Go().Iota()}
}

const (
	libcCStringSliceName = "libc.CStringSlice"
)

func (g *translator) translateMain(d *CFuncDecl) {
	osExit := g.env.Go().OsExitFunc()
	if d.Type.ArgN() == 2 {
		libcCSlice := types.NewIdent(libcCStringSliceName, g.env.FuncTT(g.env.PtrT(g.env.C().String()), types.SliceT(g.env.Go().String())))
		osArgs := types.NewIdent("os.Args", types.SliceT(g.env.Go().String()))
		argsLen := &CallExpr{Fun: FuncIdent{g.env.Go().LenFunc()}, Args: []Expr{IdentExpr{osArgs}}}
		argsPtr := &CallExpr{Fun: FuncIdent{libcCSlice}, Args: []Expr{IdentExpr{osArgs}}}
		// define main args in the function body
		args := d.Type.Args()
		argc := g.NewCDeclStmt(&CVarDecl{CVarSpec: CVarSpec{
			g:     g,
			Type:  args[0].Type(),
			Names: []*types.Ident{args[0].Name},
			Inits: []Expr{g.cCast(args[0].Type(), argsLen)},
		}})
		argv := g.NewCDeclStmt(&CVarDecl{CVarSpec: CVarSpec{
			g:     g,
			Type:  args[1].Type(),
			Names: []*types.Ident{args[1].Name},
			Inits: []Expr{g.cCast(args[1].Type(), argsPtr)},
		}})
		var stmts []CStmt
		stmts = append(stmts, argc...)
		stmts = append(stmts, argv...)
		stmts = append(stmts, d.Body.Stmts...)
		d.Body.Stmts = stmts
		d.Type = g.env.FuncT(d.Type.Return())
	}
	d.Body.Stmts, _ = cReplaceEachStmt(func(s CStmt) ([]CStmt, bool) {
		r, ok := s.(*CReturnStmt)
		if !ok {
			return []CStmt{s}, false
		}
		e := r.Expr
		if e == nil {
			e = cIntLit(0)
		}
		ex := g.NewCCallExpr(FuncIdent{osExit}, []Expr{g.cCast(g.env.Go().Int(), e)})
		return NewCExprStmt(ex), true
	}, d.Body.Stmts)
	d.Type = g.env.FuncT(nil, d.Type.Args()...)
}

func (g *translator) translate(cur string, ast *cc.AST) []GoDecl {
	decl := g.translateC(cur, ast)
	if g.conf.FixImplicitReturns {
		g.fixImplicitReturns(decl)
	}
	// adapt well-known decls like main
	decl = g.adaptMain(decl)
	// run plugin hooks
	decl = g.runASTPluginsC(cur, ast, decl)
	// flatten functions, if needed
	g.flatten(decl)
	// fix unused variables
	g.fixUnusedVars(decl)
	// convert to Go AST
	var gdecl []GoDecl
	for _, d := range decl {
		switch d := d.(type) {
		case *CFuncDecl:
			if g.conf.SkipDecl[d.Name.Name] {
				continue
			}
		case *CVarDecl:
			// TODO: skip any single one
			if len(d.Names) == 1 && g.conf.SkipDecl[d.Names[0].Name] {
				continue
			}
		case *CTypeDef:
			if g.conf.SkipDecl[d.Name().Name] {
				continue
			}
		}
		gdecl = append(gdecl, d.AsDecl()...)
	}
	return gdecl
}

func (g *translator) translateC(cur string, ast *cc.AST) []CDecl {
	g.file, g.cur = ast, strings.TrimLeft(cur, "./")

	decl := g.convertMacros(ast)

	tu := ast.TranslationUnit
	for tu != nil {
		d := tu.ExternalDeclaration
		tu = tu.TranslationUnit
		if d == nil {
			continue
		}
		var cd []CDecl
		switch d.Case {
		case cc.ExternalDeclarationFuncDef:
			cd = g.convertFuncDef(d.FunctionDefinition)
		case cc.ExternalDeclarationDecl:
			cd = g.convertDecl(d.Declaration)
		case cc.ExternalDeclarationEmpty:
			// TODO
		default:
			panic(d.Case.String() + " " + d.Position().String())
		}
		decl = append(decl, cd...)
	}
	// remove forward declarations
	m := make(map[string]CDecl)
	skip := make(map[CDecl]struct{})
	for _, d := range decl {
		switch d := d.(type) {
		case *CFuncDecl:
			d2, ok := m[d.Name.Name].(*CFuncDecl)
			if !ok {
				m[d.Name.Name] = d
				continue
			}
			if d2.Body != nil {
				skip[d] = struct{}{}
			} else {
				m[d.Name.Name] = d
				skip[d2] = struct{}{}
			}
		case *CTypeDef:
			d2, ok := m[d.Name().Name].(*CTypeDef)
			if !ok {
				m[d.Name().Name] = d
				continue
			}
			if d.Underlying() == d2.Underlying() {
				m[d.Name().Name] = d
				skip[d] = struct{}{}
			}
		}
	}
	decl2 := make([]CDecl, 0, len(decl))
	for _, d := range decl {
		if _, skip := skip[d]; skip {
			continue
		}
		decl2 = append(decl2, d)
	}
	return decl2
}
