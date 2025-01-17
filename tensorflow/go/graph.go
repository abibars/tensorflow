/*
Copyright 2016 The TensorFlow Authors. All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tensorflow

// #include "tensorflow/c/c_api.h"
//
// #include <stdlib.h>
// #include <string.h>
//
// void TF_SetAttrShapeList_Helper(TF_OperationDescription* desc,
//                                 const char* attr_name,
//                                 const int64_t* flat_dims,
//                                 const int* num_dims,
//                                 int num_shapes) {
//  const int64_t** dims =
//    (const int64_t**)malloc(sizeof(const int64_t*) * num_shapes);
//  int i = 0;
//  for (i = 0; i < num_shapes; i++) {
//    dims[i] = flat_dims;
//    if (num_dims[i] > 0) {
//      // flat_dims will be NULL iff num_shapes is 0 or all elements in num_dims are <= 0.
//      flat_dims += num_dims[i];
//    }
//  }
//  TF_SetAttrShapeList(desc, attr_name, dims, num_dims, num_shapes);
//  free(dims);
// }
import "C"

import (
	"bytes"
	"fmt"
	"io"
	"runtime"
	"sync"
	"unsafe"
)

// Graph represents a computation graph. Graphs may be shared between sessions.
type Graph struct {
	m sync.Mutex
	c *C.TF_Graph
}

// NewGraph returns a new Graph.
func NewGraph() *Graph {
	g := &Graph{c: C.TF_NewGraph()}
	runtime.SetFinalizer(g, (*Graph).finalizer)
	return g
}

func (g *Graph) finalizer() {
	g.m.Lock()
	defer g.m.Unlock()
	if g.c != nil {
		C.TF_DeleteGraph(g.c)
	}
	g.c = nil
}

// Delete deletes the graph
func (g *Graph) Delete() {
	g.finalizer()
}

// WriteTo writes out a serialized representation of g to w.
//
// Implements the io.WriterTo interface.
func (g *Graph) WriteTo(w io.Writer) (int64, error) {
	g.m.Lock()
	defer g.m.Unlock()

	buf := C.TF_NewBuffer()
	defer C.TF_DeleteBuffer(buf)

	status := newStatus()
	defer status.Delete()

	C.TF_GraphToGraphDef(g.c, buf, status.c)
	if err := status.Err(); err != nil {
		return 0, err
	}
	if buf.length > (1 << 30) {
		// For very large graphs, the writes can be chunked.
		// Punt on that for now.
		return 0, fmt.Errorf("Graph is too large to write out, Graph.WriteTo needs to be updated")
	}
	// A []byte slice backed by C memory.
	// See: https://github.com/golang/go/wiki/cgo#turning-c-arrays-into-go-slices
	length := int(buf.length)
	slice := (*[1 << 30]byte)(unsafe.Pointer(buf.data))[:length:length]
	n, err := w.Write(slice)
	return int64(n), err
}

// Import imports the nodes and edges from a serialized representation of
// another Graph into g.
//
// Names of imported nodes will be prefixed with prefix.
func (g *Graph) Import(def []byte, prefix string) error {
	return g.ImportFromReader(bytes.NewBuffer(def), len(def), prefix)
}

// ImportFromReader imports the nodes and edges from a serialized representation of
// another Graph into g.
//
// The serialized data must be provided as an io.Reader, along with (an over-estimate of)
// the size of the data in bytes, which is used to pre-allocate a C buffer.
//
// Multiple options can be specified for the newly imported nodes.
func (g *Graph) ImportFromReader(r io.Reader, size int, prefix string) error {
	cprefix := C.CString(prefix)
	defer C.free(unsafe.Pointer(cprefix))

	opts := C.TF_NewImportGraphDefOptions()
	defer C.TF_DeleteImportGraphDefOptions(opts)
	C.TF_ImportGraphDefOptionsSetPrefix(opts, cprefix)

	buf := C.TF_NewBuffer()
	defer C.TF_DeleteBuffer(buf)

	buf.data = C.malloc(C.size_t(size))
	if buf.data == nil {
		return fmt.Errorf("unable to allocate memory")
	}
	defer C.free(buf.data)

	// code adapted from io.Copy
	var nCopied int
	tmpbuf := make([]byte, 32*1024)
	for {
		nr, er := r.Read(tmpbuf)
		if nCopied+nr > size {
			return fmt.Errorf("graph def size to large for pre-allocated buffer")
		}
		if nr > 0 {
			C.memcpy(unsafe.Pointer(uintptr(buf.data)+uintptr(nCopied)), unsafe.Pointer(&tmpbuf[0]), C.size_t(nr))
		}
		nCopied += nr
		if er != nil {
			if er != io.EOF {
				return er
			}
			break
		}
	}

	// the actual number of copied bytes may be smaller than size
	buf.length = C.size_t(nCopied)

	status := newStatus()
	defer status.Delete()

	g.m.Lock()
	defer g.m.Unlock()
	C.TF_GraphImportGraphDef(g.c, buf, opts, status.c)
	if err := status.Err(); err != nil {
		return err
	}
	return nil
}

// Operation returns the Operation named name in the Graph, or nil if no such
// operation is present.
func (g *Graph) Operation(name string) *Operation {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	cop := C.TF_GraphOperationByName(g.c, cname)
	if cop == nil {
		return nil
	}
	return &Operation{cop, g}
}

// Operations returns a list of all operations in the graph
func (g *Graph) Operations() []Operation {
	var pos C.size_t = 0
	ops := []Operation{}
	for {
		cop := C.TF_GraphNextOperation(g.c, &pos)
		if cop == nil {
			break
		}
		ops = append(ops, Operation{cop, g})
	}
	return ops
}

// OpSpec is the specification of an Operation to be added to a Graph
// (using Graph.AddOperation).
type OpSpec struct {
	// Type of the operation (e.g., "Add", "MatMul").
	Type string

	// Name by which the added operation will be referred to in the Graph.
	// If omitted, defaults to Type.
	Name string

	// Inputs to this operation, which in turn must be outputs
	// of other operations already added to the Graph.
	//
	// An operation may have multiple inputs with individual inputs being
	// either a single tensor produced by another operation or a list of
	// tensors produced by multiple operations. For example, the "Concat"
	// operation takes two inputs: (1) the dimension along which to
	// concatenate and (2) a list of tensors to concatenate. Thus, for
	// Concat, len(Input) must be 2, with the first element being an Output
	// and the second being an OutputList.
	Input []Input

	// Map from attribute name to its value that will be attached to this
	// operation.
	Attrs map[string]interface{}

	// Operations that must be executed before executing the operation
	// being added.
	ControlDependencies []*Operation

	// The device on which the operation should be executed.
	// If omitted, an appropriate device will automatically be selected.
	//
	// For example, if set of "/device:GPU:0", then the operation will
	// execute on GPU #0.
	Device string

	// Other possible fields: ColocateWith.
}

// AddOperation adds an operation to g.
func (g *Graph) AddOperation(args OpSpec) (*Operation, error) {
	if args.Name == "" {
		args.Name = args.Type
	}
	cname := C.CString(args.Name)
	ctype := C.CString(args.Type)
	cdesc := C.TF_NewOperation(g.c, ctype, cname)
	C.free(unsafe.Pointer(cname))
	C.free(unsafe.Pointer(ctype))

	for _, in := range args.Input {
		switch in := in.(type) {
		case Output:
			C.TF_AddInput(cdesc, in.c())
		case OutputList:
			size := len(in)
			list := make([]C.TF_Output, size)
			for i, v := range in {
				list[i] = v.c()
			}
			if size > 0 {
				C.TF_AddInputList(cdesc, &list[0], C.int(size))
			} else {
				C.TF_AddInputList(cdesc, nil, 0)
			}
		}
	}
	for _, in := range args.ControlDependencies {
		C.TF_AddControlInput(cdesc, in.c)
	}
	status := newStatus()
	defer status.Delete()

	for name, value := range args.Attrs {
		if err := setAttr(cdesc, status, name, value); err != nil {
			// Memory leak here as the TF_OperationDescription
			// object will not be cleaned up. At the time of this
			// writing, this was next to impossible since it
			// required value to be a string tensor with
			// incorrectly encoded strings. Given this rarity, live
			// with the memory leak.  If it becomes a real problem,
			// consider adding a TF_DeleteOperationDescription
			// function to the C API.
			return nil, fmt.Errorf("%v (memory will be leaked)", err)
		}
	}
	if len(args.Device) > 0 {
		cdevice := C.CString(args.Device)
		C.TF_SetDevice(cdesc, cdevice)
		C.free(unsafe.Pointer(cdevice))
	}
	c := C.TF_FinishOperation(cdesc, status.c)
	if err := status.Err(); err != nil {
		return nil, err
	}
	return &Operation{c, g}, nil
}

func setAttr(cdesc *C.TF_OperationDescription, status *status, name string, value interface{}) error {
	cAttrName := C.CString(name)
	defer C.free(unsafe.Pointer(cAttrName))
	switch value := value.(type) {
	case string:
		cstr := C.CString(value)
		C.TF_SetAttrString(cdesc, cAttrName, unsafe.Pointer(cstr), C.size_t(len(value)))
		C.free(unsafe.Pointer(cstr))
	case []string:
		size := len(value)
		list := make([]unsafe.Pointer, size)
		lens := make([]C.size_t, size)
		for i, s := range value {
			list[i] = unsafe.Pointer(C.CString(s))
			lens[i] = C.size_t(len(s))
		}
		if size > 0 {
			C.TF_SetAttrStringList(cdesc, cAttrName, &list[0], &lens[0], C.int(size))
		} else {
			C.TF_SetAttrStringList(cdesc, cAttrName, nil, nil, 0)
		}
		for _, s := range list {
			C.free(s)
		}
	case int64:
		C.TF_SetAttrInt(cdesc, cAttrName, C.int64_t(value))
	case []int64:
		size := len(value)
		list := make([]C.int64_t, size)
		for i, v := range value {
			list[i] = C.int64_t(v)
		}
		if size > 0 {
			C.TF_SetAttrIntList(cdesc, cAttrName, &list[0], C.int(size))
		} else {
			C.TF_SetAttrIntList(cdesc, cAttrName, nil, 0)
		}
	case float32:
		C.TF_SetAttrFloat(cdesc, cAttrName, C.float(value))
	case []float32:
		size := len(value)
		list := make([]C.float, size)
		for i, v := range value {
			list[i] = C.float(v)
		}
		if size > 0 {
			C.TF_SetAttrFloatList(cdesc, cAttrName, &list[0], C.int(size))
		} else {
			C.TF_SetAttrFloatList(cdesc, cAttrName, nil, 0)
		}
	case bool:
		v := C.uchar(0)
		if value {
			v = 1
		}
		C.TF_SetAttrBool(cdesc, cAttrName, v)
	case []bool:
		size := len(value)
		list := make([]C.uchar, size)
		for i, v := range value {
			if v {
				list[i] = 1
			}
		}
		if size > 0 {
			C.TF_SetAttrBoolList(cdesc, cAttrName, &list[0], C.int(size))
		} else {
			C.TF_SetAttrBoolList(cdesc, cAttrName, nil, 0)
		}
	case DataType:
		C.TF_SetAttrType(cdesc, cAttrName, C.TF_DataType(value))
	case []DataType:
		var list *C.TF_DataType
		if len(value) > 0 {
			list = (*C.TF_DataType)(&value[0])
		}
		C.TF_SetAttrTypeList(cdesc, cAttrName, list, C.int(len(value)))
	case *Tensor:
		C.TF_SetAttrTensor(cdesc, cAttrName, value.c, status.c)
		if err := status.Err(); err != nil {
			return fmt.Errorf("bad value for attribute %q: %v", name, err)
		}
	case []*Tensor:
		size := len(value)
		list := make([]*C.TF_Tensor, size)
		for i, v := range value {
			list[i] = v.c
		}
		var plist **C.TF_Tensor
		if size > 0 {
			plist = &list[0]
		}
		C.TF_SetAttrTensorList(cdesc, cAttrName, plist, C.int(size), status.c)
		if err := status.Err(); err != nil {
			return fmt.Errorf("bad value for attribute %q: %v", name, err)
		}
	case Shape:
		ndims := C.int(value.NumDimensions())
		var dimsp *C.int64_t
		if ndims > 0 {
			dims := make([]C.int64_t, ndims)
			for i, d := range value.dims {
				dims[i] = C.int64_t(d)
			}
			dimsp = &dims[0]
		}
		C.TF_SetAttrShape(cdesc, cAttrName, dimsp, ndims)
	case []Shape:
		if len(value) == 0 {
			C.TF_SetAttrShapeList(cdesc, cAttrName, nil, nil, 0)
		} else {
			var flatDims []C.int64_t
			ndims := make([]C.int, len(value))
			for i, s := range value {
				nd := s.NumDimensions()
				ndims[i] = C.int(nd)
				for _, d := range s.dims {
					flatDims = append(flatDims, C.int64_t(d))
				}
			}
			var flatDimsp *C.int64_t
			if len(flatDims) > 0 {
				flatDimsp = &flatDims[0]
			}
			C.TF_SetAttrShapeList_Helper(cdesc, cAttrName, flatDimsp, &ndims[0], C.int(len(value)))
		}
	default:
		return fmt.Errorf("attribute %q has a type (%T) which is not valid for operation attributes", name, value)
	}
	return nil
}
