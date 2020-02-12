// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build purego appengine

package impl

import (
	"reflect"

	"google.golang.org/protobuf/internal/encoding/wire"
)

func sizeEnum(p pointer, f *coderFieldInfo, _ marshalOptions) (size int) {
	v := p.v.Elem().Int()
	return f.tagsize + wire.SizeVarint(uint64(v))
}

func appendEnum(b []byte, p pointer, f *coderFieldInfo, opts marshalOptions) ([]byte, error) {
	v := p.v.Elem().Int()
	b = wire.AppendVarint(b, f.wiretag)
	b = wire.AppendVarint(b, uint64(v))
	return b, nil
}

func consumeEnum(b []byte, p pointer, wtyp wire.Type, f *coderFieldInfo, _ unmarshalOptions) (out unmarshalOutput, err error) {
	if wtyp != wire.VarintType {
		return out, errUnknown
	}
	v, n := wire.ConsumeVarint(b)
	if n < 0 {
		return out, wire.ParseError(n)
	}
	p.v.Elem().SetInt(int64(v))
	out.n = n
	return out, nil
}

var coderEnum = pointerCoderFuncs{
	size:      sizeEnum,
	marshal:   appendEnum,
	unmarshal: consumeEnum,
}

func sizeEnumNoZero(p pointer, f *coderFieldInfo, opts marshalOptions) (size int) {
	if p.v.Elem().Int() == 0 {
		return 0
	}
	return sizeEnum(p, f, opts)
}

func appendEnumNoZero(b []byte, p pointer, f *coderFieldInfo, opts marshalOptions) ([]byte, error) {
	if p.v.Elem().Int() == 0 {
		return b, nil
	}
	return appendEnum(b, p, f, opts)
}

var coderEnumNoZero = pointerCoderFuncs{
	size:      sizeEnumNoZero,
	marshal:   appendEnumNoZero,
	unmarshal: consumeEnum,
}

func sizeEnumPtr(p pointer, f *coderFieldInfo, opts marshalOptions) (size int) {
	return sizeEnum(pointer{p.v.Elem()}, f, opts)
}

func appendEnumPtr(b []byte, p pointer, f *coderFieldInfo, opts marshalOptions) ([]byte, error) {
	return appendEnum(b, pointer{p.v.Elem()}, f, opts)
}

func consumeEnumPtr(b []byte, p pointer, wtyp wire.Type, f *coderFieldInfo, opts unmarshalOptions) (out unmarshalOutput, err error) {
	if wtyp != wire.VarintType {
		return out, errUnknown
	}
	if p.v.Elem().IsNil() {
		p.v.Elem().Set(reflect.New(p.v.Elem().Type().Elem()))
	}
	return consumeEnum(b, pointer{p.v.Elem()}, wtyp, f, opts)
}

var coderEnumPtr = pointerCoderFuncs{
	size:      sizeEnumPtr,
	marshal:   appendEnumPtr,
	unmarshal: consumeEnumPtr,
}

func sizeEnumSlice(p pointer, f *coderFieldInfo, opts marshalOptions) (size int) {
	s := p.v.Elem()
	for i, llen := 0, s.Len(); i < llen; i++ {
		size += wire.SizeVarint(uint64(s.Index(i).Int())) + f.tagsize
	}
	return size
}

func appendEnumSlice(b []byte, p pointer, f *coderFieldInfo, opts marshalOptions) ([]byte, error) {
	s := p.v.Elem()
	for i, llen := 0, s.Len(); i < llen; i++ {
		b = wire.AppendVarint(b, f.wiretag)
		b = wire.AppendVarint(b, uint64(s.Index(i).Int()))
	}
	return b, nil
}

func consumeEnumSlice(b []byte, p pointer, wtyp wire.Type, f *coderFieldInfo, opts unmarshalOptions) (out unmarshalOutput, err error) {
	s := p.v.Elem()
	if wtyp == wire.BytesType {
		b, n := wire.ConsumeBytes(b)
		if n < 0 {
			return out, wire.ParseError(n)
		}
		for len(b) > 0 {
			v, n := wire.ConsumeVarint(b)
			if n < 0 {
				return out, wire.ParseError(n)
			}
			rv := reflect.New(s.Type().Elem()).Elem()
			rv.SetInt(int64(v))
			s.Set(reflect.Append(s, rv))
			b = b[n:]
		}
		out.n = n
		return out, nil
	}
	if wtyp != wire.VarintType {
		return out, errUnknown
	}
	v, n := wire.ConsumeVarint(b)
	if n < 0 {
		return out, wire.ParseError(n)
	}
	rv := reflect.New(s.Type().Elem()).Elem()
	rv.SetInt(int64(v))
	s.Set(reflect.Append(s, rv))
	out.n = n
	return out, nil
}

var coderEnumSlice = pointerCoderFuncs{
	size:      sizeEnumSlice,
	marshal:   appendEnumSlice,
	unmarshal: consumeEnumSlice,
}

func sizeEnumPackedSlice(p pointer, f *coderFieldInfo, opts marshalOptions) (size int) {
	s := p.v.Elem()
	llen := s.Len()
	if llen == 0 {
		return 0
	}
	n := 0
	for i := 0; i < llen; i++ {
		n += wire.SizeVarint(uint64(s.Index(i).Int()))
	}
	return f.tagsize + wire.SizeBytes(n)
}

func appendEnumPackedSlice(b []byte, p pointer, f *coderFieldInfo, opts marshalOptions) ([]byte, error) {
	s := p.v.Elem()
	llen := s.Len()
	if llen == 0 {
		return b, nil
	}
	b = wire.AppendVarint(b, f.wiretag)
	n := 0
	for i := 0; i < llen; i++ {
		n += wire.SizeVarint(uint64(s.Index(i).Int()))
	}
	b = wire.AppendVarint(b, uint64(n))
	for i := 0; i < llen; i++ {
		b = wire.AppendVarint(b, uint64(s.Index(i).Int()))
	}
	return b, nil
}

var coderEnumPackedSlice = pointerCoderFuncs{
	size:      sizeEnumPackedSlice,
	marshal:   appendEnumPackedSlice,
	unmarshal: consumeEnumSlice,
}