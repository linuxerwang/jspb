/*
 * The code generator for the plugin for the protocol buffer compiler.
 * It generates Jspb code from the protocol buffer description files read
 * by the main routine.
 *
 * The implementation is a clone from htts://github.com/golang/protobuf.
 *
 * jspb uses the same 3-clause BSD license and keeps the original copyright
 * information from goprotobuf.
 */
package generator

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"strconv"
	"strings"
	"unicode"

	"github.com/golang/protobuf/proto"

	"github.com/golang/protobuf/protoc-gen-go/descriptor"
	plugin "github.com/golang/protobuf/protoc-gen-go/plugin"
)

var ErrNoPackageDefined = errors.New("no package clause in proto file.")

// Each type we import as a protocol buffer (other than FileDescriptorProto) needs
// a pointer to the FileDescriptorProto that represents it.  These types achieve that
// wrapping by placing each Proto inside a struct with the pointer to its File. The
// structs have the same names as their contents, with "Proto" removed.
// FileDescriptor is used to store the things that it points to.

// The file and package name method are common to messages and enums.
type common struct {
	file *descriptor.FileDescriptorProto // File this object comes from.
}

// PackageName is name in the package clause in the generated file.
func (c *common) PackageName() string { return uniquePackageOf(c.file) }

func (c *common) File() *descriptor.FileDescriptorProto { return c.file }

func fileIsProto3(file *descriptor.FileDescriptorProto) bool {
	return file.GetSyntax() == "proto3"
}

func (c *common) proto3() bool { return fileIsProto3(c.file) }

// Descriptor represents a protocol buffer message.
type Descriptor struct {
	common
	*descriptor.DescriptorProto
	parent   *Descriptor       // The containing message, if any.
	nested   []*Descriptor     // Inner messages, if any.
	enums    []*EnumDescriptor // Inner enums, if any.
	typename []string          // Cached typename vector.
	index    int               // The index into the container, whether the file or another message.
	path     string            // The SourceCodeInfo path as comma-separated integers.
	group    bool              // Not supported by jspb.
}

// TypeName returns the elements of the dotted type name.
// The package name is not part of this name.
func (d *Descriptor) TypeName() []string {
	if d.typename != nil {
		return d.typename
	}
	n := 0
	for parent := d; parent != nil; parent = parent.parent {
		n++
	}
	s := make([]string, n, n)
	for parent := d; parent != nil; parent = parent.parent {
		n--
		s[n] = parent.GetName()
	}
	d.typename = s
	return s
}

// EnumDescriptor describes an enum. If it's at top level, its parent will be nil.
// Otherwise it will be the descriptor of the message in which it is defined.
type EnumDescriptor struct {
	common
	*descriptor.EnumDescriptorProto
	parent   *Descriptor // The containing message, if any.
	typename []string    // Cached typename vector.
	index    int         // The index into the container, whether the file or a message.
	path     string      // The SourceCodeInfo path as comma-separated integers.
}

// TypeName returns the elements of the dotted type name.
// The package name is not part of this name.
func (e *EnumDescriptor) TypeName() (s []string) {
	if e.typename != nil {
		return e.typename
	}
	name := e.GetName()
	if e.parent == nil {
		s = make([]string, 1)
	} else {
		pname := e.parent.TypeName()
		s = make([]string, len(pname)+1)
		copy(s, pname)
	}
	s[len(s)-1] = name
	e.typename = s
	return s
}

// Everything but the last element of the full type name, CamelCased.
// The values of type Foo.Bar are call Foo_value1... not Foo_Bar_value1... .
func (e *EnumDescriptor) prefix() string {
	if e.parent == nil {
		// If the enum is not part of a message, the prefix is just the type name.
		return CamelCase(*e.Name) + "_"
	}
	typeName := e.TypeName()
	return CamelCaseSlice(typeName[0:len(typeName)-1]) + "_"
}

// The integer value of the named constant in this enumerated type.
func (e *EnumDescriptor) integerValueAsString(name string) string {
	for _, c := range e.Value {
		if c.GetName() == name {
			return fmt.Sprint(c.GetNumber())
		}
	}
	log.Fatal("cannot find value for enum constant")
	return ""
}

// ImportedDescriptor describes a type that has been publicly imported from another file.
type ImportedDescriptor struct {
	common
	o Object
}

func (id *ImportedDescriptor) TypeName() []string { return id.o.TypeName() }

// FileDescriptor describes an protocol buffer descriptor file (.proto).
// It includes slices of all the messages and enums defined within it.
// Those slices are constructed by WrapTypes.
type FileDescriptor struct {
	*descriptor.FileDescriptorProto
	desc []*Descriptor         // All the messages defined in this file.
	enum []*EnumDescriptor     // All the enums defined in this file.
	imp  []*ImportedDescriptor // All types defined in files publicly imported by this file.

	// Comments, stored as a map of path (comma-separated integers) to the comment.
	comments map[string]*descriptor.SourceCodeInfo_Location

	index int // The index of this file in the list of files to generate code for

	proto3 bool // whether to generate proto3 code for this file
}

// PackageName is the package name we'll use in the generated code to refer to this file.
func (d *FileDescriptor) PackageName() string { return uniquePackageOf(d.FileDescriptorProto) }

// goPackageName returns the package name to use in the generated jspb file.
func (d *FileDescriptor) goPackageName() (string, error) {
	// Does the file have a package clause?
	if pkg := d.GetPackage(); pkg != "" {
		return pkg, nil
	}
	return "", ErrNoPackageDefined
}

// Object is an interface abstracting the abilities shared by enums, messages, extensions and imported objects.
type Object interface {
	PackageName() string // The name we use in our output (a_b_c), possibly renamed for uniqueness.
	TypeName() []string
	File() *descriptor.FileDescriptorProto
}

// Each package name we generate must be unique. The package we're generating
// gets its own name but every other package must have a unique name that does
// not conflict in the code we generate.  These names are chosen globally (although
// they don't have to be, it simplifies things to do them globally).
func uniquePackageOf(fd *descriptor.FileDescriptorProto) string {
	s, ok := uniquePackageName[fd]
	if !ok {
		log.Fatal("internal error: no package name defined for " + fd.GetName())
	}
	return s
}

// Generator is the type whose methods generate the output, stored in the associated response structure.
type Generator struct {
	*bytes.Buffer

	Request  *plugin.CodeGeneratorRequest  // The input.
	Response *plugin.CodeGeneratorResponse // The output.

	Param     map[string]string // Command-line parameters.
	PkgPrefix string            // String to prefix to imported package file names.

	Pkg map[string]string // The names under which we import support packages

	packageName      string                     // What we're calling ourselves.
	allFiles         []*FileDescriptor          // All files in the tree
	allFilesByName   map[string]*FileDescriptor // All files by filename.
	genFiles         []*FileDescriptor          // Those files we will generate output for.
	file             *FileDescriptor            // The file we are compiling now.
	usedPackages     map[string]bool            // Names of packages used in current file.
	typeNameToObject map[string]Object          // Key is a fully-qualified name in input syntax.
	init             []string                   // Lines to emit in the init function.
	indent           string
	writeOutput      bool
}

// New creates a new generator and allocates the request and response protobufs.
func New() *Generator {
	g := new(Generator)
	g.Buffer = new(bytes.Buffer)
	g.Request = new(plugin.CodeGeneratorRequest)
	g.Response = new(plugin.CodeGeneratorResponse)
	return g
}

// Error reports a problem, including an error, and exits the program.
func (g *Generator) Error(err error, msgs ...string) {
	s := strings.Join(msgs, " ") + ":" + err.Error()
	log.Print("protoc-gen-js: error:", s)
	os.Exit(1)
}

// Fail reports a problem and exits the program.
func (g *Generator) Fail(msgs ...string) {
	s := strings.Join(msgs, " ")
	log.Print("protoc-gen-js: error:", s)
	os.Exit(1)
}

// CommandLineParameters breaks the comma-separated list of key=value pairs
// in the parameter (a member of the request protobuf) into a key/value map.
// It then sets file name mappings defined by those entries.
func (g *Generator) CommandLineParameters(parameter string) {
	g.Param = make(map[string]string)
	for _, p := range strings.Split(parameter, ",") {
		if i := strings.Index(p, "="); i < 0 {
			g.Param[p] = ""
		} else {
			g.Param[p[0:i]] = p[i+1:]
		}
	}

	for k, v := range g.Param {
		switch k {
		case "pkg_prefix":
			g.PkgPrefix = v
		}
	}
}

// DefaultPackageName returns the package name printed for the object.
// If its file is in a different package, it returns the package name we're using for this file, plus ".".
// Otherwise it returns the empty string.
func (g *Generator) DefaultPackageName(obj Object) string {
	pkg := obj.PackageName()
	if pkg == g.packageName {
		return ""
	}
	return pkg + "."
}

// For each input file, the unique package name to use, underscored.
var uniquePackageName = make(map[*descriptor.FileDescriptorProto]string)

// Package names already registered.  Key is the name from the .proto file;
// value is the name that appears in the generated code.
var pkgNamesInUse = make(map[string]bool)

// Create and remember a guaranteed unique package name for this file descriptor.
// Pkg is the candidate name.  If f is nil, it's a builtin package like "proto" and
// has no file descriptor.
func RegisterUniquePackageName(pkg string, f *FileDescriptor) string {
	// Convert dots to underscores before finding a unique alias.
	pkg = strings.Map(badToUnderscore, pkg)

	for i, orig := 1, pkg; pkgNamesInUse[pkg]; i++ {
		// It's a duplicate; must rename.
		pkg = orig + strconv.Itoa(i)
	}
	// Install it.
	pkgNamesInUse[pkg] = true
	if f != nil {
		uniquePackageName[f.FileDescriptorProto] = pkg
	}
	return pkg
}

var isJsKeyword = map[string]bool{
	"break":    true,
	"case":     true,
	"continue": true,
	"default":  true,
	"else":     true,
	"for":      true,
	"function": true,
	"goto":     true,
	"if":       true,
	"new":      true,
	"return":   true,
	"switch":   true,
	"type":     true,
	"var":      true,
}

// SetPackageNames sets the package name for this run.
// The package name must agree across all files being generated.
// It also defines unique package names for all imported files.
func (g *Generator) SetPackageNames() {
	pkg, err := g.genFiles[0].goPackageName()
	if err != nil {
		g.Error(err, g.genFiles[0].FileDescriptorProto.GetName())
	}

	// Check all files for an explicit go_package option.
	for _, f := range g.genFiles {
		thisPkg, err := f.goPackageName()
		if err != nil {
			g.Error(err, f.FileDescriptorProto.GetName())
		}
		if thisPkg != pkg {
			g.Fail("inconsistent package names:", thisPkg, pkg)
		}
	}

	g.packageName = RegisterUniquePackageName(pkg, g.genFiles[0])

AllFiles:
	for _, f := range g.allFiles {
		for _, genf := range g.genFiles {
			if f == genf {
				// In this package already.
				uniquePackageName[f.FileDescriptorProto] = g.packageName
				continue AllFiles
			}
		}
		// The file is a dependency, so we want to ignore its go_package option
		// because that is only relevant for its specific generated output.
		pkg := f.GetPackage()
		if pkg == "" {
			pkg = baseName(*f.Name)
		}
		RegisterUniquePackageName(pkg, f)
	}
}

// WrapTypes walks the incoming data, wrapping DescriptorProtos, EnumDescriptorProtos
// and FileDescriptorProtos into file-referenced objects within the Generator.
// It also creates the list of files to generate and so should be called before GenerateAllFiles.
func (g *Generator) WrapTypes() {
	g.allFiles = make([]*FileDescriptor, len(g.Request.ProtoFile))
	g.allFilesByName = make(map[string]*FileDescriptor, len(g.allFiles))
	for i, f := range g.Request.ProtoFile {
		// We must wrap the descriptors before we wrap the enums
		descs := wrapDescriptors(f)
		g.buildNestedDescriptors(descs)
		enums := wrapEnumDescriptors(f, descs)
		g.buildNestedEnums(descs, enums)
		fd := &FileDescriptor{
			FileDescriptorProto: f,
			desc:                descs,
			enum:                enums,
			proto3:              fileIsProto3(f),
		}
		extractComments(fd)
		g.allFiles[i] = fd
		g.allFilesByName[f.GetName()] = fd
	}
	for _, fd := range g.allFiles {
		fd.imp = wrapImported(fd.FileDescriptorProto, g)
	}

	g.genFiles = make([]*FileDescriptor, len(g.Request.FileToGenerate))
	for i, fileName := range g.Request.FileToGenerate {
		g.genFiles[i] = g.allFilesByName[fileName]
		if g.genFiles[i] == nil {
			g.Fail("could not find file named", fileName)
		}
		g.genFiles[i].index = i
	}
	g.Response.File = make([]*plugin.CodeGeneratorResponse_File, len(g.genFiles))
}

// Scan the descriptors in this file.  For each one, build the slice of nested descriptors
func (g *Generator) buildNestedDescriptors(descs []*Descriptor) {
	for _, desc := range descs {
		if len(desc.NestedType) != 0 {
			for _, nest := range descs {
				if nest.parent == desc {
					desc.nested = append(desc.nested, nest)
				}
			}
			if len(desc.nested) != len(desc.NestedType) {
				g.Fail("internal error: nesting failure for", desc.GetName())
			}
		}
	}
}

func (g *Generator) buildNestedEnums(descs []*Descriptor, enums []*EnumDescriptor) {
	for _, desc := range descs {
		if len(desc.EnumType) != 0 {
			for _, enum := range enums {
				if enum.parent == desc {
					desc.enums = append(desc.enums, enum)
				}
			}
			if len(desc.enums) != len(desc.EnumType) {
				g.Fail("internal error: enum nesting failure for", desc.GetName())
			}
		}
	}
}

// Construct the Descriptor
func newDescriptor(desc *descriptor.DescriptorProto, parent *Descriptor, file *descriptor.FileDescriptorProto, index int) *Descriptor {
	d := &Descriptor{
		common:          common{file},
		DescriptorProto: desc,
		parent:          parent,
		index:           index,
	}
	if parent == nil {
		d.path = fmt.Sprintf("%d,%d", messagePath, index)
	} else {
		d.path = fmt.Sprintf("%s,%d,%d", parent.path, messageMessagePath, index)
	}

	// The only way to distinguish a group from a message is whether
	// the containing message has a TYPE_GROUP field that matches.
	if parent != nil {
		parts := d.TypeName()
		if file.Package != nil {
			parts = append([]string{*file.Package}, parts...)
		}
		exp := "." + strings.Join(parts, ".")
		for _, field := range parent.Field {
			if field.GetType() == descriptor.FieldDescriptorProto_TYPE_GROUP && field.GetTypeName() == exp {
				d.group = true
				break
			}
		}
	}

	return d
}

// Return a slice of all the Descriptors defined within this file
func wrapDescriptors(file *descriptor.FileDescriptorProto) []*Descriptor {
	sl := make([]*Descriptor, 0, len(file.MessageType)+10)
	for i, desc := range file.MessageType {
		sl = wrapThisDescriptor(sl, desc, nil, file, i)
	}
	return sl
}

// Wrap this Descriptor, recursively
func wrapThisDescriptor(sl []*Descriptor, desc *descriptor.DescriptorProto, parent *Descriptor, file *descriptor.FileDescriptorProto, index int) []*Descriptor {
	sl = append(sl, newDescriptor(desc, parent, file, index))
	me := sl[len(sl)-1]
	for i, nested := range desc.NestedType {
		sl = wrapThisDescriptor(sl, nested, me, file, i)
	}
	return sl
}

// Construct the EnumDescriptor
func newEnumDescriptor(desc *descriptor.EnumDescriptorProto, parent *Descriptor, file *descriptor.FileDescriptorProto, index int) *EnumDescriptor {
	ed := &EnumDescriptor{
		common:              common{file},
		EnumDescriptorProto: desc,
		parent:              parent,
		index:               index,
	}
	if parent == nil {
		ed.path = fmt.Sprintf("%d,%d", enumPath, index)
	} else {
		ed.path = fmt.Sprintf("%s,%d,%d", parent.path, messageEnumPath, index)
	}
	return ed
}

// Return a slice of all the EnumDescriptors defined within this file
func wrapEnumDescriptors(file *descriptor.FileDescriptorProto, descs []*Descriptor) []*EnumDescriptor {
	sl := make([]*EnumDescriptor, 0, len(file.EnumType)+10)
	// Top-level enums.
	for i, enum := range file.EnumType {
		sl = append(sl, newEnumDescriptor(enum, nil, file, i))
	}
	// Enums within messages. Enums within embedded messages appear in the outer-most message.
	for _, nested := range descs {
		for i, enum := range nested.EnumType {
			sl = append(sl, newEnumDescriptor(enum, nested, file, i))
		}
	}
	return sl
}

// Return a slice of all the types that are publicly imported into this file.
func wrapImported(file *descriptor.FileDescriptorProto, g *Generator) (sl []*ImportedDescriptor) {
	for _, index := range file.PublicDependency {
		df := g.fileByName(file.Dependency[index])
		for _, d := range df.desc {
			if d.GetOptions().GetMapEntry() {
				continue
			}
			sl = append(sl, &ImportedDescriptor{common{file}, d})
		}
		for _, e := range df.enum {
			sl = append(sl, &ImportedDescriptor{common{file}, e})
		}
	}
	return
}

func extractComments(file *FileDescriptor) {
	file.comments = make(map[string]*descriptor.SourceCodeInfo_Location)
	for _, loc := range file.GetSourceCodeInfo().GetLocation() {
		if loc.LeadingComments == nil {
			continue
		}
		var p []string
		for _, n := range loc.Path {
			p = append(p, strconv.Itoa(int(n)))
		}
		file.comments[strings.Join(p, ",")] = loc
	}
}

// BuildTypeNameMap builds the map from fully qualified type names to objects.
// The key names for the map come from the input data, which puts a period at the beginning.
// It should be called after SetPackageNames and before GenerateAllFiles.
func (g *Generator) BuildTypeNameMap() {
	g.typeNameToObject = make(map[string]Object)
	for _, f := range g.allFiles {
		// The names in this loop are defined by the proto world, not us, so the
		// package name may be empty.  If so, the dotted package name of X will
		// be ".X"; otherwise it will be ".pkg.X".
		dottedPkg := "." + f.GetPackage()
		if dottedPkg != "." {
			dottedPkg += "."
		}
		for _, enum := range f.enum {
			name := dottedPkg + dottedSlice(enum.TypeName())
			g.typeNameToObject[name] = enum
		}
		for _, desc := range f.desc {
			name := dottedPkg + dottedSlice(desc.TypeName())
			g.typeNameToObject[name] = desc
		}
	}
}

// ObjectNamed, given a fully-qualified input type name as it appears in the input data,
// returns the descriptor for the message or enum with that name.
func (g *Generator) ObjectNamed(typeName string) Object {
	o, ok := g.typeNameToObject[typeName]
	if !ok {
		g.Fail("can't find object with type", typeName)
	}

	// If the file of this object isn't a direct dependency of the current file,
	// or in the current file, then this object has been publicly imported into
	// a dependency of the current file.
	// We should return the ImportedDescriptor object for it instead.
	direct := *o.File().Name == *g.file.Name
	if !direct {
		for _, dep := range g.file.Dependency {
			if *g.fileByName(dep).Name == *o.File().Name {
				direct = true
				break
			}
		}
	}
	if !direct {
		found := false
	Loop:
		for _, dep := range g.file.Dependency {
			df := g.fileByName(*g.fileByName(dep).Name)
			for _, td := range df.imp {
				if td.o == o {
					// Found it!
					o = td
					found = true
					break Loop
				}
			}
		}
		if !found {
			log.Printf("protoc-gen-js: WARNING: failed finding publicly imported dependency for %v, used in %v", typeName, *g.file.Name)
		}
	}

	return o
}

// P prints the arguments to the generated output.  It handles strings and int32s, plus
// handling indirections because they may be *string, etc.
func (g *Generator) P(str ...interface{}) {
	if !g.writeOutput {
		return
	}
	g.WriteString(g.indent)
	for _, v := range str {
		switch s := v.(type) {
		case string:
			g.WriteString(s)
		case *string:
			g.WriteString(*s)
		case bool:
			fmt.Fprintf(g, "%t", s)
		case *bool:
			fmt.Fprintf(g, "%t", *s)
		case int:
			fmt.Fprintf(g, "%d", s)
		case *int32:
			fmt.Fprintf(g, "%d", *s)
		case *int64:
			fmt.Fprintf(g, "%d", *s)
		case float64:
			fmt.Fprintf(g, "%g", s)
		case *float64:
			fmt.Fprintf(g, "%g", *s)
		default:
			g.Fail(fmt.Sprintf("unknown type in printer: %T", v))
		}
	}
	g.WriteByte('\n')
}

// addInitf stores the given statement to be printed inside the file's init function.
// The statement is given as a format specifier and arguments.
func (g *Generator) addInitf(stmt string, a ...interface{}) {
	g.init = append(g.init, fmt.Sprintf(stmt, a...))
}

// In Indents the output one tab stop.
func (g *Generator) In() { g.indent += "\t" }

// Out unindents the output one tab stop.
func (g *Generator) Out() {
	if len(g.indent) > 0 {
		g.indent = g.indent[1:]
	}
}

// GenerateAllFiles generates the output for all the files we're outputting.
func (g *Generator) GenerateAllFiles() {
	// Generate the output. The generator runs for every file, even the files
	// that we don't generate output for, so that we can collate the full list
	// of exported symbols to support public imports.
	genFileMap := make(map[*FileDescriptor]bool, len(g.genFiles))
	for _, file := range g.genFiles {
		genFileMap[file] = true
	}
	i := 0
	for _, file := range g.allFiles {
		g.Reset()
		g.writeOutput = genFileMap[file]
		g.generate(file)
		if !g.writeOutput {
			continue
		}
		g.Response.File[i] = new(plugin.CodeGeneratorResponse_File)
		g.Response.File[i].Name = proto.String(jsFileName(*file.Name))
		g.Response.File[i].Content = proto.String(g.String())
		i++
	}
}

// FileOf return the FileDescriptor for this FileDescriptorProto.
func (g *Generator) FileOf(fd *descriptor.FileDescriptorProto) *FileDescriptor {
	for _, file := range g.allFiles {
		if file.FileDescriptorProto == fd {
			return file
		}
	}
	g.Fail("could not find file in table:", fd.GetName())
	return nil
}

// Fill the response protocol buffer with the generated output for all the files we're
// supposed to generate.
func (g *Generator) generate(file *FileDescriptor) {
	g.file = g.FileOf(file.FileDescriptorProto)
	g.usedPackages = make(map[string]bool)

	for _, enum := range g.file.enum {
		g.generateEnum(enum)
	}
	for _, desc := range g.file.desc {
		// Don't generate virtual messages for maps.
		if desc.GetOptions().GetMapEntry() {
			continue
		}
		g.generateMessage(desc)
	}

	// Generate header and imports last, though they appear first in the output.
	rem := g.Buffer
	g.Buffer = new(bytes.Buffer)
	g.generateHeader()
	g.generateProvides()
	g.P()
	g.generateRequires()
	g.P()
	if !g.writeOutput {
		return
	}
	g.Write(rem.Bytes())
}

// Generate the header, including package definition
func (g *Generator) generateHeader() {
	g.P("// Code generated by protoc-gen-js.")
	g.P("// source: ", g.file.Name)
	g.P("// DO NOT EDIT!")
	g.P()

	if g.file.index == 0 {
		// Generate file overview docs.
		g.P("/**")
		g.P(" * @fileoverview Generated protocol buffers in Javascript.")
		g.P(" */")
		g.P()
	}
}

// PrintComments prints any comments from the source .proto file.
// The path is a comma-separated list of integers.
// It returns an indication of whether any comments were printed.
// See descriptor.proto for its format.
func (g *Generator) PrintComments(path string) bool {
	if !g.writeOutput {
		return false
	}
	if loc, ok := g.file.comments[path]; ok {
		text := strings.TrimSuffix(loc.GetLeadingComments(), "\n")
		for _, line := range strings.Split(text, "\n") {
			g.P(" * ", strings.TrimPrefix(line, " "))
		}
		return true
	}
	return false
}

func (g *Generator) fileByName(filename string) *FileDescriptor {
	return g.allFilesByName[filename]
}

// weak returns whether the ith import of the current file is a weak import.
func (g *Generator) weak(i int32) bool {
	for _, j := range g.file.WeakDependency {
		if j == i {
			return true
		}
	}
	return false
}

// Generate the provides.
func (g *Generator) generateProvides() {
	for _, enum := range g.file.enum {
		// The full type name
		typeName := enum.TypeName()
		// The full type name, CamelCased.
		ccTypeName := CamelCaseSlice(typeName)

		if g.PkgPrefix != "" {
			g.P("goog.provide('", g.PkgPrefix, ".", g.file.GetPackage(), ".", ccTypeName, "');")
		} else {
			g.P("goog.provide('", g.file.GetPackage(), ".", ccTypeName, "');")
		}
	}
	for _, desc := range g.file.desc {
		// Don't generate virtual messages for maps.
		if desc.GetOptions().GetMapEntry() {
			continue
		}

		// The full type name
		typeName := desc.TypeName()
		// The full type name, CamelCased.
		ccTypeName := CamelCaseSlice(typeName)

		if g.PkgPrefix != "" {
			g.P("goog.provide('", g.PkgPrefix, ".", g.file.GetPackage(), ".", ccTypeName, "');")
		} else {
			g.P("goog.provide('", g.file.GetPackage(), ".", ccTypeName, "');")
		}
	}
}

// Generate the requires.
func (g *Generator) generateRequires() {

AllFiles:
	for _, f := range g.allFiles {
		for _, genf := range g.genFiles {
			if f == genf {
				continue AllFiles
			}
		}
		// The file is a dependency, generate goog.requie().
		pkg := f.GetPackage()
		if pkg == "" {
			pkg = baseName(*f.Name)
		}

		for _, enum := range f.enum {
			// The full type name
			typeName := enum.TypeName()
			// The full type name, CamelCased.
			ccTypeName := CamelCaseSlice(typeName)

			if g.PkgPrefix != "" {
				g.P("goog.provide('", g.PkgPrefix, ".", f.GetPackage(), ".", ccTypeName, "');")
			} else {
				g.P("goog.provide('", f.GetPackage(), ".", ccTypeName, "');")
			}
		}
		for _, desc := range f.desc {
			// Don't generate virtual messages for maps.
			if desc.GetOptions().GetMapEntry() {
				continue
			}

			// The full type name
			typeName := desc.TypeName()
			// The full type name, CamelCased.
			ccTypeName := CamelCaseSlice(typeName)

			g.P("goog.require('", g.PkgPrefix, ".", f.GetPackage(), ".", ccTypeName, "');")
		}
		g.P("goog.require('goog.array');")
	}
}

// Generate the enum definitions for this EnumDescriptor.
func (g *Generator) generateEnum(enum *EnumDescriptor) {
	// The full type name
	typeName := enum.TypeName()
	// The full type name, CamelCased.
	ccTypeName := CamelCaseSlice(typeName)

	g.P("/**")
	g.PrintComments(enum.path)
	g.P(" * @enum {number}")
	g.P(" */")
	if g.PkgPrefix != "" {
		g.P(g.PkgPrefix, ".", g.file.GetPackage(), ".", ccTypeName, " = {")
	} else {
		g.P(g.file.GetPackage(), ".", ccTypeName, " = {")
	}
	n := len(enum.GetValue())
	for i, v := range enum.GetValue() {
		g.In()
		if i < n-1 {
			g.P(fmt.Sprintf("%s: %d,", v.GetName(), v.GetNumber()))
		} else {
			g.P(fmt.Sprintf("%s: %d", v.GetName(), v.GetNumber()))
		}
		g.Out()
	}
	g.P("};")
	g.P()
}

// TypeName is the printed name appropriate for an item. If the object is in the current file,
// TypeName drops the package name and underscores the rest.
// Otherwise the object is from another package; and the result is the underscored
// package name followed by the item name.
// The result always has an initial capital.
func (g *Generator) TypeName(obj Object) string {
	return g.DefaultPackageName(obj) + CamelCaseSlice(obj.TypeName())
}

// TypeNameWithPackage is like TypeName, but always includes the package
// name even if the object is in our own package.
func (g *Generator) TypeNameWithPackage(obj Object) string {
	return obj.PackageName() + CamelCaseSlice(obj.TypeName())
}

// JsType returns a string representing the type name, element type (empty
// if it's not an array of messages), and the wire type.
func (g *Generator) JsType(message *Descriptor, field *descriptor.FieldDescriptorProto) (typ, eleTyp string, wire string) {
	// TODO: Options.
	switch *field.Type {
	case descriptor.FieldDescriptorProto_TYPE_DOUBLE:
		typ, wire = "number", "fixed64"
	case descriptor.FieldDescriptorProto_TYPE_FLOAT:
		typ, wire = "number", "fixed32"
	case descriptor.FieldDescriptorProto_TYPE_INT64:
		typ, wire = "number", "varint"
	case descriptor.FieldDescriptorProto_TYPE_UINT64:
		typ, wire = "number", "varint"
	case descriptor.FieldDescriptorProto_TYPE_INT32:
		typ, wire = "number", "varint"
	case descriptor.FieldDescriptorProto_TYPE_UINT32:
		typ, wire = "number", "varint"
	case descriptor.FieldDescriptorProto_TYPE_FIXED64:
		typ, wire = "number", "fixed64"
	case descriptor.FieldDescriptorProto_TYPE_FIXED32:
		typ, wire = "number", "fixed32"
	case descriptor.FieldDescriptorProto_TYPE_BOOL:
		typ, wire = "boolean", "varint"
	case descriptor.FieldDescriptorProto_TYPE_STRING:
		typ, wire = "string", "bytes"
	case descriptor.FieldDescriptorProto_TYPE_MESSAGE:
		desc := g.ObjectNamed(field.GetTypeName())
		typ, wire = g.TypeName(desc), "bytes"
	case descriptor.FieldDescriptorProto_TYPE_BYTES:
		typ, wire = "string", "bytes"
	case descriptor.FieldDescriptorProto_TYPE_ENUM:
		desc := g.ObjectNamed(field.GetTypeName())
		typ, wire = g.TypeName(desc), "varint"
	case descriptor.FieldDescriptorProto_TYPE_SFIXED32:
		typ, wire = "number", "fixed32"
	case descriptor.FieldDescriptorProto_TYPE_SFIXED64:
		typ, wire = "number", "fixed64"
	case descriptor.FieldDescriptorProto_TYPE_SINT32:
		typ, wire = "number", "zigzag32"
	case descriptor.FieldDescriptorProto_TYPE_SINT64:
		typ, wire = "number", "zigzag64"
	default:
		g.Fail("unknown type for", field.GetName())
	}
	if isRepeated(field) {
		if *field.Type == descriptor.FieldDescriptorProto_TYPE_MESSAGE {
			eleTyp = g.PkgPrefix + "." + message.PackageName() + "." + typ
			typ = "Array.<" + eleTyp + ">"
		} else if *field.Type == descriptor.FieldDescriptorProto_TYPE_ENUM {
			typ = "Array.<number>"
		} else {
			typ = "Array.<" + typ + ">"
		}
	} else if *field.Type == descriptor.FieldDescriptorProto_TYPE_MESSAGE ||
		*field.Type == descriptor.FieldDescriptorProto_TYPE_ENUM {
		typ = g.PkgPrefix + "." + message.PackageName() + "." + typ
	}
	return
}

// Method names that may be generated.  Fields with these names get an
// underscore appended.
var methodNames = [...]string{
	"Reset",
	"String",
	"ProtoMessage",
	"Marshal",
	"Unmarshal",
	"ExtensionRangeArray",
	"ExtensionMap",
	"Descriptor",
}

// Generate the type and default constant definitions for this Descriptor.
func (g *Generator) generateMessage(message *Descriptor) {
	// The full type name
	typeName := message.TypeName()

	// The full type name, CamelCased.
	ccTypeName := CamelCaseSlice(typeName)

	usedNames := make(map[string]bool)
	fieldNames := make(map[*descriptor.FieldDescriptorProto]string)
	fieldGetterNames := make(map[*descriptor.FieldDescriptorProto]string)
	fieldTypes := make(map[*descriptor.FieldDescriptorProto]string)
	mapFieldTypes := make(map[*descriptor.FieldDescriptorProto]string)

	oneofFieldName := make(map[int32]string)                           // indexed by oneof_index field of FieldDescriptorProto
	oneofDisc := make(map[int32]string)                                // name of discriminator method
	oneofTypeName := make(map[*descriptor.FieldDescriptorProto]string) // without star
	oneofInsertPoints := make(map[int32]int)                           // oneof_index => offset of g.Buffer

	g.P("/**")
	g.PrintComments(message.path)
	g.P(" * @param {Object} jsonData The JSON data.")
	g.P(" * @constructor")
	g.P(" */")
	if g.PkgPrefix != "" {
		g.P(g.PkgPrefix, ".", message.PackageName(), ".", ccTypeName, " = function(jsonData) {")
	} else {
		g.P(message.PackageName(), ".", ccTypeName, " = function(jsonData) {")
	}
	g.In()
	g.P("/**")
	g.P(" * @private {Object}")
	g.P(" */")
	g.P("this.jsonData_ = jsonData;")
	g.Out()
	g.P("};")
	g.P()

	// Generate getJsonData().
	g.P("/**")
	g.P(" * @return {Object} The JSON data.")
	g.P(" */")
	if g.PkgPrefix != "" {
		g.P(g.PkgPrefix, ".", message.PackageName(), ".", ccTypeName, ".prototype.getJsonData", " = function() {")
	} else {
		g.P(message.PackageName(), ".", ccTypeName, ".getJsonData = function() {")
	}
	g.In()
	g.P("return this.jsonData_;")
	g.Out()
	g.P("};")
	g.P()

	// Default constants
	defNames := make(map[*descriptor.FieldDescriptorProto]string)
	for _, field := range message.Field {
		typename, _, _ := g.JsType(message, field)

		def := field.GetDefaultValue()
		if def != "" {
			defNames[field] = strconv.Quote(def)
			continue
		}

		switch {
		case typename == "boolean":
			def = "false"
		case typename == "string":
			def = "''"
		case typename == "number":
			def = "0"
		case strings.HasPrefix(typename, "Array."):
			def = "[]"
		case *field.Type == descriptor.FieldDescriptorProto_TYPE_ENUM:
			def = "0"
		default:
			def = "undefined"
		}
		defNames[field] = def
	}

	// allocNames finds a conflict-free variation of the given strings,
	// consistently mutating their suffixes.
	// It returns the same number of strings.
	allocNames := func(ns ...string) []string {
	Loop:
		for {
			for _, n := range ns {
				if usedNames[n] {
					for i := range ns {
						ns[i] += "_"
					}
					continue Loop
				}
			}
			for _, n := range ns {
				usedNames[n] = true
			}
			return ns
		}
	}

	for i, field := range message.Field {
		// Allocate the getter and the field at the same time so name
		// collisions create field/method consistent names.
		// TODO: This allocation occurs based on the order of the fields
		// in the proto file, meaning that a change in the field
		// ordering can change generated Method/Field names.
		base := CamelCase(*field.Name)
		ns := allocNames(base, "get"+base, "set"+base)
		fieldName, fieldGetterName, fieldSetterName := ns[0], ns[1], ns[2]
		typename, eleTyp, _ := g.JsType(message, field)

		fieldNames[field] = fieldName
		fieldGetterNames[field] = fieldGetterName

		oneof := field.OneofIndex != nil
		if oneof && oneofFieldName[*field.OneofIndex] == "" {
			odp := message.OneofDecl[int(*field.OneofIndex)]
			fname := allocNames(CamelCase(odp.GetName()))[0]

			// This is the first field of a oneof we haven't seen before.
			// Generate the union field.
			com := g.PrintComments(fmt.Sprintf("%s,%d,%d", message.path, messageOneofPath, *field.OneofIndex))
			if com {
				g.P("//")
			}
			g.P("// Types that are valid to be assigned to ", fname, ":")
			// Generate the rest of this comment later,
			// when we've computed any disambiguation.
			oneofInsertPoints[*field.OneofIndex] = g.Buffer.Len()

			dname := "is" + ccTypeName + "_" + fname
			oneofFieldName[*field.OneofIndex] = fname
			oneofDisc[*field.OneofIndex] = dname
			tag := `protobuf_oneof:"` + odp.GetName() + `"`
			g.P(fname, " ", dname, " `", tag, "`")
		}

		if *field.Type == descriptor.FieldDescriptorProto_TYPE_MESSAGE {
			desc := g.ObjectNamed(field.GetTypeName())
			if d, ok := desc.(*Descriptor); ok && d.GetOptions().GetMapEntry() {
				// Figure out the Go types and tags for the key and value types.
				keyField, valField := d.Field[0], d.Field[1]
				keyType, _, _ := g.JsType(d, keyField)
				valType, _, _ := g.JsType(d, valField)

				// We don't use stars, except for message-typed values.
				// Message and enum types are the only two possibly foreign types used in maps,
				// so record their use. They are not permitted as map keys.
				keyType = strings.TrimPrefix(keyType, "*")
				switch *valField.Type {
				case descriptor.FieldDescriptorProto_TYPE_ENUM:
					valType = strings.TrimPrefix(valType, "*")
				default:
					valType = strings.TrimPrefix(valType, "*")
				}

				typename = fmt.Sprintf("map[%s]%s", keyType, valType)
				mapFieldTypes[field] = typename // record for the getter generation
			}
		}

		fieldTypes[field] = typename

		if oneof {
			tname := ccTypeName + "_" + fieldName
			// It is possible for this to collide with a message or enum
			// nested in this message. Check for collisions.
			for {
				ok := true
				for _, desc := range message.nested {
					if CamelCaseSlice(desc.TypeName()) == tname {
						ok = false
						break
					}
				}
				for _, enum := range message.enums {
					if CamelCaseSlice(enum.TypeName()) == tname {
						ok = false
						break
					}
				}
				if !ok {
					tname += "_"
					continue
				}
				break
			}

			oneofTypeName[field] = tname
			continue
		}

		// Generate getters.

		g.P("/**")
		g.PrintComments(fmt.Sprintf("%s,%d,%d", message.path, messageFieldPath, i))
		g.P(" * @return {", typename, "}")
		g.P(" */")
		if g.PkgPrefix != "" {
			g.P(g.PkgPrefix, ".", message.PackageName(), ".", ccTypeName, ".prototype.", fieldGetterName, " = function() {")
		} else {
			g.P(message.PackageName(), ".", ccTypeName, ".prototype.", fieldGetterName, " = function() {")
		}
		g.In()
		if *field.Type == descriptor.FieldDescriptorProto_TYPE_MESSAGE {
			g.P(fmt.Sprintf("if (this.%s_) {", field.GetName()))
			g.In()
			g.P(fmt.Sprintf("return this.%s_;", field.GetName()))
			g.Out()
			g.P("}")
			g.P("var v = this.jsonData_[\"" + field.GetName() + "\"];")
			g.P("if (v) {")
			g.In()
			if eleTyp != "" {
				g.P(fmt.Sprintf("/** @private {%s} */", typename))
				g.P(fmt.Sprintf("this.%s_ = [];", field.GetName()))
				g.P("goog.array.forEach(v, function(__item, __index) {")
				g.In()
				g.P(fmt.Sprintf("this.%s_.push(new %s(__item));", field.GetName(), eleTyp))
				g.Out()
				g.P("}, this);")
				g.P(fmt.Sprintf("return this.%s_;", field.GetName()))
			} else {
				g.P(fmt.Sprintf("/** @private {%s} */", typename))
				g.P(fmt.Sprintf("this.%s_ = new %s(v);", field.GetName(), typename))
				g.P(fmt.Sprintf("return this.%s_;", field.GetName()))
			}
			g.Out()
			g.P("}")
			g.P("return " + defNames[field] + ";")
		} else {
			g.P(fmt.Sprintf("return this.jsonData_[\"%s\"] || ", field.GetName()), defNames[field], ";")
		}
		g.Out()
		g.P("};")
		g.P()

		// Generate setters.

		g.P("/**")
		g.PrintComments(fmt.Sprintf("%s,%d,%d", message.path, messageFieldPath, i))
		g.P(" * @param {", typename, "} ", field.GetName(), " The ", field.GetName()+".")
		g.P(" */")
		if g.PkgPrefix != "" {
			g.P(g.PkgPrefix, ".", message.PackageName(), ".", ccTypeName, ".prototype.", fieldSetterName, " = function(", field.GetName(), ") {")
		} else {
			g.P(message.PackageName(), ".", ccTypeName, ".prototype.", fieldSetterName, " = function(", field.GetName(), ") {")
		}
		g.In()
		if *field.Type == descriptor.FieldDescriptorProto_TYPE_MESSAGE {
			if eleTyp != "" {
				g.P(fmt.Sprintf("var __data = %s.getJsonData();", field.GetName()))
				g.P("if (__data) {")
				g.In()
				g.P("var __array = [];")
				g.P("goog.array.forEach(__data, function(__item, __index) {")
				g.In()
				g.P("__array.push(__item.getJsonData());")
				g.Out()
				g.P("}, this);")
				g.P(fmt.Sprintf("this.jsonData_[\"%s\"] = __array;", field.GetName()))
				g.Out()
				g.P("} else {")
				g.In()
				g.P(fmt.Sprintf("this.jsonData_[\"%s\"] = [];", field.GetName()))
				g.Out()
				g.P("}")
			} else {
				g.P("this.jsonData_[\"" + field.GetName() + "\"] = " + field.GetName() + ".getJsonData();")
			}
			g.P(fmt.Sprintf("this.%s_ = undefined;", field.GetName()))
		} else {
			g.P("this.jsonData_[\"" + field.GetName() + "\"] = " + field.GetName() + ";")
		}
		g.Out()
		g.P("};")
		g.P()
	}
	g.Out()

	// Update g.Buffer to list valid oneof types.
	// We do this down here, after we've disambiguated the oneof type names.
	// We go in reverse order of insertion point to avoid invalidating offsets.
	for oi := int32(len(message.OneofDecl)); oi >= 0; oi-- {
		ip := oneofInsertPoints[oi]
		all := g.Buffer.Bytes()
		rem := all[ip:]
		g.Buffer = bytes.NewBuffer(all[:ip:ip]) // set cap so we don't scribble on rem
		for _, field := range message.Field {
			if field.OneofIndex == nil || *field.OneofIndex != oi {
				continue
			}
			g.P("//\t*", oneofTypeName[field])
		}
		g.Buffer.Write(rem)
	}

	// Oneof per-field types, discriminants and getters.
	//
	// Generate unexported named types for the discriminant interfaces.
	// We shouldn't have to do this, but there was (~19 Aug 2015) a compiler/linker bug
	// that was triggered by using anonymous interfaces here.
	// TODO: Revisit this and consider reverting back to anonymous interfaces.
	if len(message.OneofDecl) > 0 {
		for oi := range message.OneofDecl {
			dname := oneofDisc[int32(oi)]
			g.P("type ", dname, " interface { ", dname, "() }")
		}
		g.P()
	}
	if len(message.Field) > 0 {
		for _, field := range message.Field {
			if field.OneofIndex == nil {
				continue
			}
			g.P("type ", oneofTypeName[field], " struct{ ", fieldNames[field], " ", fieldTypes[field], "}")
		}
		g.P()
		for _, field := range message.Field {
			if field.OneofIndex == nil {
				continue
			}
			g.P("func (*", oneofTypeName[field], ") ", oneofDisc[*field.OneofIndex], "() {}")
		}
		g.P()
	}
	if len(message.OneofDecl) > 0 {
		for oi := range message.OneofDecl {
			fname := oneofFieldName[int32(oi)]
			g.P("func (m *", ccTypeName, ") Get", fname, "() ", oneofDisc[int32(oi)], " {")
			g.P("if m != nil { return m.", fname, " }")
			g.P("return nil")
			g.P("}")
		}
		g.P()
	}
}

// And now lots of helper functions.

// Is c an ASCII lower-case letter?
func isASCIILower(c byte) bool {
	return 'a' <= c && c <= 'z'
}

// Is c an ASCII digit?
func isASCIIDigit(c byte) bool {
	return '0' <= c && c <= '9'
}

// CamelCase returns the CamelCased name.
// If there is an interior underscore followed by a lower case letter,
// drop the underscore and convert the letter to upper case.
// There is a remote possibility of this rewrite causing a name collision,
// but it's so remote we're prepared to pretend it's nonexistent - since the
// C++ generator lowercases names, it's extremely unlikely to have two fields
// with different capitalizations.
// In short, _my_field_name_2 becomes XMyFieldName_2.
func CamelCase(s string) string {
	if s == "" {
		return ""
	}
	t := make([]byte, 0, 32)
	i := 0
	if s[0] == '_' {
		// Need a capital letter; drop the '_'.
		t = append(t, 'X')
		i++
	}
	// Invariant: if the next letter is lower case, it must be converted
	// to upper case.
	// That is, we process a word at a time, where words are marked by _ or
	// upper case letter. Digits are treated as words.
	for ; i < len(s); i++ {
		c := s[i]
		if c == '_' && i+1 < len(s) && isASCIILower(s[i+1]) {
			continue // Skip the underscore in s.
		}
		if isASCIIDigit(c) {
			t = append(t, c)
			continue
		}
		// Assume we have a letter now - if not, it's a bogus identifier.
		// The next word is a sequence of characters that must start upper case.
		if isASCIILower(c) {
			c ^= ' ' // Make it a capital letter.
		}
		t = append(t, c) // Guaranteed not lower case.
		// Accept lower case sequence that follows.
		for i+1 < len(s) && isASCIILower(s[i+1]) {
			i++
			t = append(t, s[i])
		}
	}
	return string(t)
}

// CamelCaseSlice is like CamelCase, but the argument is a slice of strings to
// be joined with "_".
func CamelCaseSlice(elem []string) string { return CamelCase(strings.Join(elem, "_")) }

// dottedSlice turns a sliced name into a dotted name.
func dottedSlice(elem []string) string { return strings.Join(elem, ".") }

// Given a .proto file name, return the output name for the generated JavaScript program.
func jsFileName(name string) string {
	ext := path.Ext(name)
	if ext == ".proto" || ext == ".protodevel" {
		name = name[0 : len(name)-len(ext)]
	}
	return name + ".pb.js"
}

// Is this field repeated?
func isRepeated(field *descriptor.FieldDescriptorProto) bool {
	return field.Label != nil && *field.Label == descriptor.FieldDescriptorProto_LABEL_REPEATED
}

// badToUnderscore is the mapping function used to generate Go names from package names,
// which can be dotted in the input .proto file.  It replaces non-identifier characters such as
// dot or dash with underscore.
func badToUnderscore(r rune) rune {
	if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
		return r
	}
	return '_'
}

// baseName returns the last path element of the name, with the last dotted suffix removed.
func baseName(name string) string {
	// First, find the last element
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	// Now drop the suffix
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[0:i]
	}
	return name
}

// The SourceCodeInfo message describes the location of elements of a parsed
// .proto file by way of a "path", which is a sequence of integers that
// describe the route from a FileDescriptorProto to the relevant submessage.
// The path alternates between a field number of a repeated field, and an index
// into that repeated field. The constants below define the field numbers that
// are used.
//
// See descriptor.proto for more information about this.
const (
	// tag numbers in FileDescriptorProto
	packagePath = 2 // package
	messagePath = 4 // message_type
	enumPath    = 5 // enum_type
	// tag numbers in DescriptorProto
	messageFieldPath   = 2 // field
	messageMessagePath = 3 // nested_type
	messageEnumPath    = 4 // enum_type
	messageOneofPath   = 8 // oneof_decl
	// tag numbers in EnumDescriptorProto
	enumValuePath = 2 // value
)
