// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package codegen contains shared utilities for generating code.
package codegen

import (
	"bufio"
	"bytes"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/imports"
	"tailscale.com/util/mak"
	"tailscale.com/util/must"
)

// LoadTypes returns all named types in pkgName, keyed by their type name.
func LoadTypes(buildTags string, pkgName string) (*packages.Package, map[string]*types.Named, error) {
	cfg := &packages.Config{
		Mode:  packages.NeedTypes | packages.NeedTypesInfo | packages.NeedSyntax | packages.NeedName,
		Tests: false,
	}
	if buildTags != "" {
		cfg.BuildFlags = []string{"-tags=" + buildTags}
	}

	pkgs, err := packages.Load(cfg, pkgName)
	if err != nil {
		return nil, nil, err
	}
	if len(pkgs) != 1 {
		return nil, nil, fmt.Errorf("wrong number of packages: %d", len(pkgs))
	}
	pkg := pkgs[0]
	return pkg, namedTypes(pkg), nil
}

// HasNoClone reports whether the provided tag has `codegen:noclone`.
func HasNoClone(structTag string) bool {
	val := reflect.StructTag(structTag).Get("codegen")
	for _, v := range strings.Split(val, ",") {
		if v == "noclone" {
			return true
		}
	}
	return false
}

const copyrightHeader = `// Copyright (c) %d Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

`

const genAndPackageHeader = `// Code generated by %v; DO NOT EDIT.

package %s
`

func NewImportTracker(thisPkg *types.Package) *ImportTracker {
	return &ImportTracker{
		thisPkg: thisPkg,
	}
}

// ImportTracker provides a mechanism to track and build import paths.
type ImportTracker struct {
	thisPkg  *types.Package
	packages map[string]bool
}

func (it *ImportTracker) Import(pkg string) {
	if pkg != "" && !it.packages[pkg] {
		mak.Set(&it.packages, pkg, true)
	}
}

func (it *ImportTracker) qualifier(pkg *types.Package) string {
	if it.thisPkg == pkg {
		return ""
	}
	it.Import(pkg.Path())
	// TODO(maisem): handle conflicts?
	return pkg.Name()
}

// QualifiedName returns the string representation of t in the package.
func (it *ImportTracker) QualifiedName(t types.Type) string {
	return types.TypeString(t, it.qualifier)
}

// Write prints all the tracked imports in a single import block to w.
func (it *ImportTracker) Write(w io.Writer) {
	fmt.Fprintf(w, "import (\n")
	for s := range it.packages {
		fmt.Fprintf(w, "\t%q\n", s)
	}
	fmt.Fprintf(w, ")\n\n")
}

func writeHeader(w io.Writer, tool, pkg string, copyrightYear int) {
	if copyrightYear != 0 {
		fmt.Fprintf(w, copyrightHeader, copyrightYear)
	}
	fmt.Fprintf(w, genAndPackageHeader, tool, pkg)
}

// WritePackageFile adds a file with the provided imports and contents to package.
// The tool param is used to identify the tool that generated package file.
func WritePackageFile(tool string, pkg *packages.Package, path string, copyrightYear int, it *ImportTracker, contents *bytes.Buffer) error {
	buf := new(bytes.Buffer)
	writeHeader(buf, tool, pkg.Name, copyrightYear)
	it.Write(buf)
	if _, err := buf.Write(contents.Bytes()); err != nil {
		return err
	}
	return writeFormatted(buf.Bytes(), path)
}

// writeFormatted writes code to path.
// It runs gofmt on it before writing;
// if gofmt fails, it writes code unchanged.
// Errors can include I/O errors and gofmt errors.
//
// The advantage of always writing code to path,
// even if gofmt fails, is that it makes debugging easier.
// The code can be long, but you need it in order to debug.
// It is nicer to work with it in a file than a terminal.
// It is also easier to interpret gofmt errors
// with an editor providing file and line numbers.
func writeFormatted(code []byte, path string) error {
	out, fmterr := imports.Process(path, code, &imports.Options{
		Comments:   true,
		TabIndent:  true,
		TabWidth:   8,
		FormatOnly: true, // fancy gofmt only
	})
	if fmterr != nil {
		out = code
	}
	ioerr := os.WriteFile(path, out, 0644)
	// Prefer I/O errors. They're usually easier to fix,
	// and until they're fixed you can't do much else.
	if ioerr != nil {
		return ioerr
	}
	if fmterr != nil {
		return fmt.Errorf("%s:%v", path, fmterr)
	}
	return nil
}

// namedTypes returns all named types in pkg, keyed by their type name.
func namedTypes(pkg *packages.Package) map[string]*types.Named {
	nt := make(map[string]*types.Named)
	for _, file := range pkg.Syntax {
		for _, d := range file.Decls {
			decl, ok := d.(*ast.GenDecl)
			if !ok || decl.Tok != token.TYPE {
				continue
			}
			for _, s := range decl.Specs {
				spec, ok := s.(*ast.TypeSpec)
				if !ok {
					continue
				}
				typeNameObj, ok := pkg.TypesInfo.Defs[spec.Name]
				if !ok {
					continue
				}
				typ, ok := typeNameObj.Type().(*types.Named)
				if !ok {
					continue
				}
				nt[spec.Name.Name] = typ
			}
		}
	}
	return nt
}

// AssertStructUnchanged generates code that asserts at compile time that type t is unchanged.
// thisPkg is the package containing t.
// tname is the named type corresponding to t.
// ctx is a single-word context for this assertion, such as "Clone".
// If non-nil, AssertStructUnchanged will add elements to imports
// for each package path that the caller must import for the returned code to compile.
func AssertStructUnchanged(t *types.Struct, tname, ctx string, it *ImportTracker) []byte {
	buf := new(bytes.Buffer)
	w := func(format string, args ...any) {
		fmt.Fprintf(buf, format+"\n", args...)
	}
	w("// A compilation failure here means this code must be regenerated, with the command at the top of this file.")
	w("var _%s%sNeedsRegeneration = %s(struct {", tname, ctx, tname)

	for i := 0; i < t.NumFields(); i++ {
		fname := t.Field(i).Name()
		ft := t.Field(i).Type()
		if IsInvalid(ft) {
			continue
		}
		qname := it.QualifiedName(ft)
		w("\t%s %s", fname, qname)
	}

	w("}{})\n")
	return buf.Bytes()
}

// IsInvalid reports whether the provided type is invalid. It is used to allow
// codegeneration to run even when the target files have build errors or are
// missing views.
func IsInvalid(t types.Type) bool {
	return t.String() == "invalid type"
}

// ContainsPointers reports whether typ contains any pointers,
// either explicitly or implicitly.
// It has special handling for some types that contain pointers
// that we know are free from memory aliasing/mutation concerns.
func ContainsPointers(typ types.Type) bool {
	switch typ.String() {
	case "time.Time":
		// time.Time contains a pointer that does not need copying
		return false
	case "inet.af/netip.Addr", "net/netip.Addr", "net/netip.Prefix", "net/netip.AddrPort":
		return false
	}
	switch ft := typ.Underlying().(type) {
	case *types.Array:
		return ContainsPointers(ft.Elem())
	case *types.Chan:
		return true
	case *types.Interface:
		return true // a little too broad
	case *types.Map:
		return true
	case *types.Pointer:
		return true
	case *types.Slice:
		return true
	case *types.Struct:
		for i := 0; i < ft.NumFields(); i++ {
			if ContainsPointers(ft.Field(i).Type()) {
				return true
			}
		}
	}
	return false
}

// IsViewType reports whether the provided typ is a View.
func IsViewType(typ types.Type) bool {
	t, ok := typ.Underlying().(*types.Struct)
	if !ok {
		return false
	}
	if t.NumFields() != 1 {
		return false
	}
	return t.Field(0).Name() == "ж"
}

// CopyrightYear reports the greatest copyright year in non-generated *.go files
// in the current directory, for use in the copyright line of generated code.
//
// It panics on I/O error, as it's assumed this is only being used by "go
// generate" or GitHub actions.
//
// TODO(bradfitz,dgentry): determine what heuristic to use for all this: latest
// year, earliest, none? don't list years at all? IANAL. Get advice of others.
// For now we just want to unbreak the tree. See Issue 6865.
func CopyrightYear(dir string) (year int) {
	files, err := os.ReadDir(dir)
	if err != nil {
		panic(err)
	}
	rxYear := regexp.MustCompile(`^// Copyright \(c\) (20\d{2}) `)
	rxGenerated := regexp.MustCompile(`^// Code generated .* DO NOT EDIT\.$`)
Files:
	for _, f := range files {
		name := f.Name()
		if !f.Type().IsRegular() ||
			strings.HasPrefix(name, ".") || // includes emacs noise
			!strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_clone.go") ||
			strings.HasSuffix(name, "_view.go") ||
			strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			panic(err)
		}
		bs := bufio.NewScanner(bytes.NewReader(src))
		for bs.Scan() {
			line := bs.Bytes()
			if m := rxYear.FindSubmatch(line); m != nil {
				if y := must.Get(strconv.Atoi(string(m[1]))); y > year {
					year = y
				}
				continue
			}
			if rxGenerated.Match(line) {
				continue Files
			}
		}
	}
	return year
}
