package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"unicode"
)

var (
	typeNames    = flag.String("type", "", "comma-separated list of type names; must be set")
	output       = flag.String("output", "", "output file name; default srcdir/<type>_athena.sql")
	templatePath = flag.String("template", "./templates/presto.tpl", "template file")
)

func Usage() {
	io.WriteString(os.Stderr, usageText)
	flag.PrintDefaults()
}

const usageText = `athena-schema
Example:
	athena-schema -type=User [directory]
	athena-schema -type=User files... # Must be a single package

Flags:
`

func main() {
	log.SetFlags(0)
	log.SetPrefix("athena_schema: ")
	flag.Usage = Usage
	flag.Parse()
	if len(*typeNames) == 0 {
		flag.Usage()
		os.Exit(2)
	}

	types := strings.Split(*typeNames, ",")

	// We accept either one directory or a list of files. Which do we have?
	args := flag.Args()
	if len(args) == 0 {
		// Default: process whole package in current directory.
		args = []string{"."}
	}

	// Parse the package once.
	var dir string
	g := Generator{
		Tables: make([]Table, 0, len(types)),
	}
	if len(args) == 1 && isDirectory(args[0]) {
		dir = args[0]
		g.parsePackageDir(args[0])
	} else {
		dir = filepath.Dir(args[0])
		g.parsePackageFiles(args)
	}

	for _, typeName := range types {
		g.generate(typeName)
	}

	// Format the output.
	src := g.format(*templatePath)

	// Write to file.
	outputName := *output
	if outputName == "" {
		baseName := fmt.Sprintf("%s_athena.sql", types[0])
		outputName = filepath.Join(dir, strings.ToLower(baseName))
	}

	err := ioutil.WriteFile(outputName, src, 0644)
	if err != nil {
		log.Fatalf("writing output: %s", err)
	}
}

func isDirectory(name string) bool {
	info, err := os.Stat(name)
	if err != nil {
		log.Fatal(err)
	}
	return info.IsDir()
}

type Generator struct {
	pkg    *Package
	Tables []Table
}

type Table struct {
	Name    string
	Columns []Column
}

type Column struct {
	Name string
	Type string
}

type Package struct {
	dir      string
	name     string
	defs     map[*ast.Ident]types.Object
	files    []*File
	typesPkg *types.Package
}

type File struct {
	ast *ast.File

	// state for each type
	typeName string
	tables   []Table
}

func (g *Generator) parsePackageDir(directory string) {
	pkg, err := build.Default.ImportDir(directory, 0)
	if err != nil {
		log.Fatalf("cannot process directory %s: %s", directory, err)
	}
	var names []string
	names = append(names, pkg.GoFiles...)
	names = append(names, pkg.CgoFiles...)
	names = append(names, pkg.SFiles...)
	names = prefixDirectory(directory, names)
	g.parsePackage(directory, names, nil)
}

func (g *Generator) parsePackageFiles(names []string) {
	g.parsePackage(".", names, nil)
}

func prefixDirectory(directory string, names []string) []string {
	if directory == "." {
		return names
	}
	ret := make([]string, len(names))
	for i, name := range names {
		ret[i] = filepath.Join(directory, name)
	}
	return ret
}

func (g *Generator) parsePackage(directory string, names []string, text interface{}) {
	var files []*File
	var astFiles []*ast.File
	g.pkg = new(Package)
	fs := token.NewFileSet()
	for _, name := range names {
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		parsedFile, err := parser.ParseFile(fs, name, text, parser.ParseComments)
		if err != nil {
			log.Fatalf("parsing package: %s: %s", name, err)
		}
		astFiles = append(astFiles, parsedFile)
		files = append(files, &File{
			ast: parsedFile,
		})
	}
	if len(astFiles) == 0 {
		log.Fatalf("%s: no buildable Go files", directory)
	}
	g.pkg.name = astFiles[0].Name.Name
	g.pkg.files = files
	g.pkg.dir = directory

	g.pkg.check(fs, astFiles)
}

func (pkg *Package) check(fs *token.FileSet, astFiles []*ast.File) {
	pkg.defs = make(map[*ast.Ident]types.Object)
	config := types.Config{Importer: importer.Default(), FakeImportC: true}
	info := &types.Info{
		Defs: pkg.defs,
	}
	typesPkg, err := config.Check(pkg.dir, fs, astFiles, info)
	if err != nil {
		log.Fatalf("checking package: %s", err)
	}
	pkg.typesPkg = typesPkg
}

func (g *Generator) generate(typeName string) {
	for _, file := range g.pkg.files {
		// pass state to file
		file.typeName = typeName
		file.tables = nil
		if file.ast != nil {
			ast.Inspect(file.ast, file.createTable)
			g.Tables = append(g.Tables, file.tables...)
		}
	}
}

func (f *File) createTable(node ast.Node) bool {
	decl, ok := node.(*ast.GenDecl)
	if !ok || decl.Tok != token.TYPE {
		return true
	}
	for _, spec := range decl.Specs {
		typeSpec, ok := spec.(*ast.TypeSpec)
		if !ok {
			return true
		}
		structName := typeSpec.Name.Name
		if structName != f.typeName {
			return true
		}

		structType, ok := typeSpec.Type.(*ast.StructType)

		table := Table{
			Name:    CamelToSnake(structName),
			Columns: make([]Column, 0, len(structType.Fields.List)),
		}

		for _, field := range structType.Fields.List {
			name := CamelToSnake(field.Names[0].Name)
			fieldType := types.ExprString(field.Type)

			sqlType, supportedType := SQLTypeMap[fieldType]

			// overwirte by tags
			if field.Tag != nil {
				tags := reflect.StructTag(field.Tag.Value[1 : len(field.Tag.Value)-1])
				jsonTag, ok := tags.Lookup("json")
				if ok {
					name = jsonTag
				}

				athenaType, ok := tags.Lookup("athena")
				if ok {
					sqlType = athenaType
				} else if !supportedType {
					log.Printf("no support field type: %s", fieldType)
				}
			}

			column := Column{
				Name: name,
				Type: sqlType,
			}

			table.Columns = append(table.Columns, column)
		}
		f.tables = append(f.tables, table)
	}
	return false
}

type Tmpl struct {
	CmdLog      string
	PackageName string
	Tables      []Table
}

func (g *Generator) format(templatePath string) []byte {
	tmpl, err := template.ParseFiles(templatePath)

	if err != nil {
		log.Fatalf("Template %v path is not exist", templatePath)
	}

	newbytes := bytes.NewBufferString("")
	t := &Tmpl{
		CmdLog:      fmt.Sprintf("athena_schema %s", strings.Join(os.Args[1:], " ")),
		PackageName: g.pkg.name,
		Tables:      g.Tables,
	}

	err = tmpl.Execute(newbytes, t)

	if err != nil {
		log.Fatalf("%v", err)
	}

	tplcontent, err := ioutil.ReadAll(newbytes)

	return tplcontent
}

var SQLTypeMap = map[string]string{
	"bool":      "BOOLEAN",
	"string":    "STRING",
	"int":       "INT",
	"int8":      "INT",
	"int16":     "INT",
	"int32":     "INT",
	"int64":     "INT",
	"uint8":     "INT",
	"uint16":    "INT",
	"uint32":    "INT",
	"uint64":    "INT",
	"float32":   "FLOAT",
	"float64":   "DOUBLE",
	"time.Time": "timestamp",
}

func CamelToSnake(s string) string {
	var result string
	var words []string
	var lastPos int
	rs := []rune(s)

	for i := 0; i < len(rs); i++ {
		if i > 0 && unicode.IsUpper(rs[i]) {
			if initialism := startsWithInitialism(s[lastPos:]); initialism != "" {
				words = append(words, initialism)

				i += len(initialism) - 1
				lastPos = i
				continue
			}

			words = append(words, s[lastPos:i])
			lastPos = i
		}
	}

	// append the last word
	if s[lastPos:] != "" {
		words = append(words, s[lastPos:])
	}

	for k, word := range words {
		if k > 0 {
			result += "_"
		}

		result += strings.ToLower(word)
	}

	return result
}

// startsWithInitialism returns the initialism if the given string begins with it
func startsWithInitialism(s string) string {
	var initialism string
	// the longest initialism is 5 char, the shortest 2
	for i := 1; i <= 5; i++ {
		if len(s) > i-1 && commonInitialisms[s[:i]] {
			initialism = s[:i]
		}
	}
	return initialism
}

// commonInitialisms, taken from
// https://github.com/golang/lint/blob/206c0f020eba0f7fbcfbc467a5eb808037df2ed6/lint.go#L731
var commonInitialisms = map[string]bool{
	"ACL":   true,
	"API":   true,
	"ASCII": true,
	"CPU":   true,
	"CSS":   true,
	"DNS":   true,
	"EOF":   true,
	"ETA":   true,
	"GPU":   true,
	"GUID":  true,
	"HTML":  true,
	"HTTP":  true,
	"HTTPS": true,
	"ID":    true,
	"IP":    true,
	"JSON":  true,
	"LHS":   true,
	"OS":    true,
	"QPS":   true,
	"RAM":   true,
	"RHS":   true,
	"RPC":   true,
	"SLA":   true,
	"SMTP":  true,
	"SQL":   true,
	"SSH":   true,
	"TCP":   true,
	"TLS":   true,
	"TTL":   true,
	"UDP":   true,
	"UI":    true,
	"UID":   true,
	"UUID":  true,
	"URI":   true,
	"URL":   true,
	"UTF8":  true,
	"VM":    true,
	"XML":   true,
	"XMPP":  true,
	"XSRF":  true,
	"XSS":   true,
	"OAuth": true,
}
