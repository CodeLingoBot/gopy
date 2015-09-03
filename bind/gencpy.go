// Copyright 2015 The go-python Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package bind

import (
	"go/token"
	"path/filepath"
)

const (
	cPreamble = `/*
  C stubs for package %[1]s.
  gopy gen -lang=python %[1]s

  File is generated by gopy gen. Do not edit.
*/

#ifdef _POSIX_C_SOURCE
#undef _POSIX_C_SOURCE
#endif

#include "Python.h"
#include "structmember.h"
#include "memoryobject.h"
#include "bufferobject.h"

// header exported from 'go tool cgo'
#include "%[3]s.h"

#if PY_VERSION_HEX > 0x03000000
#error "Python-3 is not yet supported by gopy"
#endif

// descriptor for calls placed to the wrapped go package
#define cgopy_seq_DESCRIPTOR %[1]q

// --- gopy object model ---

struct _gopy_object;

// empty interface converter
typedef GoInterface (*gopy_efacefunc)(struct _gopy_object *);


// proxy for all go values
struct _gopy_object {
	PyObject_HEAD
	void *go; /* handle to address of go value */
	gopy_efacefunc eface;
};

typedef struct _gopy_object gopy_object;

// --- gopy object model ---


// helpers for cgopy

#define def_cnv(name, c2py, py2c, gotype) \
	static int \
	cgopy_cnv_py2c_ ## name(PyObject *o, gotype *addr) { \
		*addr = py2c(o); \
		return 1;	\
	} \
	\
	static PyObject* \
	cgopy_cnv_c2py_ ## name(gotype *addr) { \
		return c2py(*addr); \
	} 

#if (GOINTBITS == 4)
	def_cnv( int,  PyLong_FromLong,         PyLong_AsLong,         GoInt)
	def_cnv(uint,  PyLong_FromUnsignedLong, PyLong_AsUnsignedLong, GoUint)
#else
	def_cnv( int,  PyInt_FromLong, PyInt_AsLong, GoInt)
	def_cnv(uint,  PyInt_FromLong, PyInt_AsLong, GoUint)
#endif

def_cnv(  int8, PyInt_FromLong, PyInt_AsLong, GoInt8)
def_cnv( int16, PyInt_FromLong, PyInt_AsLong, GoInt16)
def_cnv( int32, PyInt_FromLong, PyInt_AsLong, GoInt32)
def_cnv( int64, PyLong_FromLong, PyLong_AsLong, GoInt64)
def_cnv(uint8,  PyInt_FromLong, PyInt_AsLong, GoUint8)
def_cnv(uint16, PyInt_FromLong, PyInt_AsLong, GoUint16)
def_cnv(uint32, PyInt_FromLong, PyInt_AsLong, GoUint32)
def_cnv(uint64, PyLong_FromUnsignedLong, PyLong_AsUnsignedLong, GoUint64)

def_cnv(float64, PyFloat_FromDouble, PyFloat_AsDouble, GoFloat64)

#undef def_cnv

static int
cgopy_cnv_py2c_bool(PyObject *o, GoUint8 *addr) {
	*addr = (o == Py_True) ? 1 : 0;
	return 1;
}

static PyObject*
cgopy_cnv_c2py_bool(GoUint8 *addr) {
	long v = *addr;
	return PyBool_FromLong(v);
}

static int
cgopy_cnv_py2c_string(PyObject *o, GoString *addr) {
	const char *str = PyString_AsString(o);
	if (str == NULL) {
		return 0;
	}
	*addr = _cgopy_GoString((char*)str);
	return 1;
}

static PyObject*
cgopy_cnv_c2py_string(GoString *addr) {
	const char *str = _cgopy_CString(*addr);
	PyObject *pystr = PyString_FromString(str);
	free((void*)str);
	return pystr;
}

static int
cgopy_cnv_py2c_float32(PyObject *o, GoFloat32 *addr) {
	GoFloat32 v = PyFloat_AsDouble(o);
	*addr = v;
	return 1;
}

static PyObject*
cgopy_cnv_c2py_float32(GoFloat32 *addr) {
	GoFloat64 v = *addr;
	return PyFloat_FromDouble(v);
}

static int
cgopy_cnv_py2c_complex64(PyObject *o, GoComplex64 *addr) {
	Py_complex v = PyComplex_AsCComplex(o);
	*addr = v.real + v.imag * _Complex_I;
	return 1;
}

static PyObject*
cgopy_cnv_c2py_complex64(GoComplex64 *addr) {
	return PyComplex_FromDoubles(creal(*addr), cimag(*addr));
}

static int
cgopy_cnv_py2c_complex128(PyObject *o, GoComplex128 *addr) {
	Py_complex v = PyComplex_AsCComplex(o);
	*addr = v.real + v.imag * _Complex_I;
	return 1;
}

static PyObject*
cgopy_cnv_c2py_complex128(GoComplex128 *addr) {
	return PyComplex_FromDoubles(creal(*addr), cimag(*addr));
}
`
)

type cpyGen struct {
	decl *printer
	impl *printer

	fset *token.FileSet
	pkg  *Package
	err  ErrorList

	lang int // c-python api version (2,3)
}

func (g *cpyGen) gen() error {

	g.genPreamble()

	// first, process slices, arrays
	{
		names := g.pkg.syms.names()
		for _, n := range names {
			sym := g.pkg.syms.sym(n)
			if !sym.isType() {
				continue
			}
			g.genType(sym)
		}
	}

	// then, process structs
	for _, s := range g.pkg.structs {
		g.genStruct(s)
	}

	// expose ctors at module level
	// FIXME(sbinet): attach them to structs?
	// -> problem is if one has 2 or more ctors with exactly the same signature.
	for _, s := range g.pkg.structs {
		for _, ctor := range s.ctors {
			g.genFunc(ctor)
		}
	}

	for _, f := range g.pkg.funcs {
		g.genFunc(f)
	}

	for _, c := range g.pkg.consts {
		g.genConst(c)
	}

	for _, v := range g.pkg.vars {
		g.genVar(v)
	}

	g.impl.Printf("\n/* functions for package %s */\n", g.pkg.pkg.Name())
	g.impl.Printf("static PyMethodDef cpy_%s_methods[] = {\n", g.pkg.pkg.Name())
	g.impl.Indent()
	for _, f := range g.pkg.funcs {
		name := f.GoName()
		//obj := scope.Lookup(name)
		g.impl.Printf("{%[1]q, %[2]s, METH_VARARGS, %[3]q},\n",
			name, "cpy_func_"+f.ID(), f.Doc(),
		)
	}
	// expose ctors at module level
	// FIXME(sbinet): attach them to structs?
	// -> problem is if one has 2 or more ctors with exactly the same signature.
	for _, s := range g.pkg.structs {
		for _, f := range s.ctors {
			name := f.GoName()
			//obj := scope.Lookup(name)
			g.impl.Printf("{%[1]q, %[2]s, METH_VARARGS, %[3]q},\n",
				name, "cpy_func_"+f.ID(), f.Doc(),
			)
		}
	}

	for _, c := range g.pkg.consts {
		name := c.GoName()
		g.impl.Printf("{%[1]q, %[2]s, METH_VARARGS, %[3]q},\n",
			"Get"+name, "cpy_func_"+c.id+"_get", c.Doc(),
		)
	}

	for _, v := range g.pkg.vars {
		name := v.Name()
		g.impl.Printf("{%[1]q, %[2]s, METH_VARARGS, %[3]q},\n",
			"Get"+name, "cpy_func_"+v.id+"_get", v.doc,
		)
		g.impl.Printf("{%[1]q, %[2]s, METH_VARARGS, %[3]q},\n",
			"Set"+name, "cpy_func_"+v.id+"_set", v.doc,
		)
	}

	g.impl.Printf("{NULL, NULL, 0, NULL}        /* Sentinel */\n")
	g.impl.Outdent()
	g.impl.Printf("};\n\n")

	g.impl.Printf("PyMODINIT_FUNC\ninit%[1]s(void)\n{\n", g.pkg.pkg.Name())
	g.impl.Indent()
	g.impl.Printf("PyObject *module = NULL;\n\n")

	g.impl.Printf("/* make sure Cgo is loaded and initialized */\n")
	g.impl.Printf("cgo_pkg_%[1]s_init();\n\n", g.pkg.pkg.Name())

	for _, n := range g.pkg.syms.names() {
		sym := g.pkg.syms.sym(n)
		if !sym.isType() {
			continue
		}
		g.impl.Printf(
			"if (PyType_Ready(&%sType) < 0) { return; }\n",
			sym.cpyname,
		)
	}

	g.impl.Printf("module = Py_InitModule3(%[1]q, cpy_%[1]s_methods, %[2]q);\n\n",
		g.pkg.pkg.Name(),
		g.pkg.doc.Doc,
	)

	for _, n := range g.pkg.syms.names() {
		sym := g.pkg.syms.sym(n)
		if !sym.isType() {
			continue
		}
		g.impl.Printf("Py_INCREF(&%sType);\n", sym.cpyname)
		g.impl.Printf("PyModule_AddObject(module, %q, (PyObject*)&%sType);\n\n",
			sym.goname,
			sym.cpyname,
		)
	}
	g.impl.Outdent()
	g.impl.Printf("}\n\n")

	if len(g.err) > 0 {
		return g.err
	}

	return nil
}

func (g *cpyGen) genConst(o Const) {
	g.genFunc(o.f)
}

func (g *cpyGen) genVar(v Var) {

	id := g.pkg.Name() + "_" + v.Name()
	doc := v.doc
	{
		res := []*Var{newVar(g.pkg, v.GoType(), "ret", v.Name(), doc)}
		sig := newSignature(g.pkg, nil, nil, res)
		fget := Func{
			pkg:  g.pkg,
			sig:  sig,
			typ:  nil,
			name: v.Name(),
			id:   id + "_get",
			doc:  "returns " + g.pkg.Name() + "." + v.Name(),
			ret:  v.GoType(),
			err:  false,
		}
		g.genFunc(fget)
	}
	{
		params := []*Var{newVar(g.pkg, v.GoType(), "arg", v.Name(), doc)}
		sig := newSignature(g.pkg, nil, params, nil)
		fset := Func{
			pkg:  g.pkg,
			sig:  sig,
			typ:  nil,
			name: v.Name(),
			id:   id + "_set",
			doc:  "sets " + g.pkg.Name() + "." + v.Name(),
			ret:  nil,
			err:  false,
		}
		g.genFunc(fset)
	}
}

func (g *cpyGen) genPreamble() {
	n := g.pkg.pkg.Name()
	g.decl.Printf(cPreamble, n, g.pkg.pkg.Path(), filepath.Base(n))
}
