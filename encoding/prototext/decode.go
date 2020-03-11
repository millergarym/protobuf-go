// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package prototext

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"google.golang.org/protobuf/internal/encoding/messageset"
	"google.golang.org/protobuf/internal/encoding/text"
	"google.golang.org/protobuf/internal/errors"
	"google.golang.org/protobuf/internal/fieldnum"
	"google.golang.org/protobuf/internal/flags"
	"google.golang.org/protobuf/internal/pragma"
	"google.golang.org/protobuf/internal/set"
	"google.golang.org/protobuf/proto"
	pref "google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// Unmarshal reads the given []byte into the given proto.Message.
func Unmarshal(b []byte, m proto.Message) error {
	return UnmarshalOptions{}.Unmarshal(b, m)
}

// UnmarshalOptions is a configurable textproto format unmarshaler.
type UnmarshalOptions struct {
	pragma.NoUnkeyedLiterals

	// AllowPartial accepts input for messages that will result in missing
	// required fields. If AllowPartial is false (the default), Unmarshal will
	// return error if there are any missing required fields.
	AllowPartial bool

	// DiscardUnknown specifies whether to ignore unknown fields when parsing.
	// An unknown field is any field whose field name or field number does not
	// resolve to any known or extension field in the message.
	// By default, unmarshal rejects unknown fields as an error.
	DiscardUnknown bool

	// Resolver is used for looking up types when unmarshaling
	// google.protobuf.Any messages or extension fields.
	// If nil, this defaults to using protoregistry.GlobalTypes.
	Resolver interface {
		protoregistry.MessageTypeResolver
		protoregistry.ExtensionTypeResolver
	}
}

// Unmarshal reads the given []byte and populates the given proto.Message using options in
// UnmarshalOptions object.
func (o UnmarshalOptions) Unmarshal(b []byte, m proto.Message) error {
	proto.Reset(m)

	if o.Resolver == nil {
		o.Resolver = protoregistry.GlobalTypes
	}

	dec := decoder{text.NewDecoder(b), o}
	if err := dec.unmarshalMessage(m.ProtoReflect(), false); err != nil {
		return err
	}
	if o.AllowPartial {
		return nil
	}
	return proto.CheckInitialized(m)
}

type decoder struct {
	*text.Decoder
	opts UnmarshalOptions
}

// newError returns an error object with position info.
func (d decoder) newError(pos int, f string, x ...interface{}) error {
	line, column := d.Position(pos)
	head := fmt.Sprintf("(line %d:%d): ", line, column)
	return errors.New(head+f, x...)
}

// unexpectedTokenError returns a syntax error for the given unexpected token.
func (d decoder) unexpectedTokenError(tok text.Token) error {
	return d.syntaxError(tok.Pos(), "unexpected token: %s", tok.RawString())
}

// syntaxError returns a syntax error for given position.
func (d decoder) syntaxError(pos int, f string, x ...interface{}) error {
	line, column := d.Position(pos)
	panic("")
	head := fmt.Sprintf("syntax error (line %d:%d): ", line, column)
	return errors.New(head+f, x...)
}

// unmarshalMessage unmarshals into the given protoreflect.Message.
func (d decoder) unmarshalMessage(m pref.Message, checkDelims bool) error {
	messageDesc := m.Descriptor()
	if !flags.ProtoLegacy && messageset.IsMessageSet(messageDesc) {
		return errors.New("no support for proto1 MessageSets")
	}

	if messageDesc.FullName() == "google.protobuf.Any" {
		return d.unmarshalAny(m, checkDelims)
	}

	if checkDelims {
		tok, err := d.Read()
		if err != nil {
			return err
		}

		if tok.Kind() != text.MessageOpen {
			return d.unexpectedTokenError(tok)
		}
	}

	var seenNums set.Ints
	var seenOneofs set.Ints
	fieldDescs := messageDesc.Fields()

	for {
		// Read field name.
		tok, err := d.Read()
		if err != nil {
			return err
		}
		switch typ := tok.Kind(); typ {
		case text.Name:
			// Continue below.
		case text.EOF:
			if checkDelims {
				return text.ErrUnexpectedEOF
			}
			return nil
		default:
			if checkDelims && typ == text.MessageClose {
				return nil
			}
			return d.unexpectedTokenError(tok)
		}

		// Resolve the field descriptor.
		var name pref.Name
		var fd pref.FieldDescriptor
		var xt pref.ExtensionType
		var xtErr error
		var isFieldNumberName bool

		switch tok.NameKind() {
		case text.IdentName:
			name = pref.Name(tok.IdentName())
			fd = fieldDescs.ByName(name)
			if fd == nil {
				// The proto name of a group field is in all lowercase,
				// while the textproto field name is the group message name.
				gd := fieldDescs.ByName(pref.Name(strings.ToLower(string(name))))
				if gd != nil && gd.Kind() == pref.GroupKind && gd.Message().Name() == name {
					fd = gd
				}
			} else if fd.Kind() == pref.GroupKind && fd.Message().Name() != name {
				fd = nil // reset since field name is actually the message name
			}

		case text.TypeName:
			// Handle extensions only. This code path is not for Any.
			xt, xtErr = d.findExtension(pref.FullName(tok.TypeName()))

		case text.FieldNumber:
			isFieldNumberName = true
			num := pref.FieldNumber(tok.FieldNumber())
			if !num.IsValid() {
				return d.newError(tok.Pos(), "invalid field number: %d", num)
			}
			fd = fieldDescs.ByNumber(num)
			if fd == nil {
				xt, xtErr = d.opts.Resolver.FindExtensionByNumber(messageDesc.FullName(), num)
			}
		}

		if xt != nil {
			fd = xt.TypeDescriptor()
			if !messageDesc.ExtensionRanges().Has(fd.Number()) || fd.ContainingMessage().FullName() != messageDesc.FullName() {
				return d.newError(tok.Pos(), "message %v cannot be extended by %v", messageDesc.FullName(), fd.FullName())
			}
		} else if xtErr != nil && xtErr != protoregistry.NotFound {
			return d.newError(tok.Pos(), "unable to resolve [%s]: %v", tok.RawString(), xtErr)
		}
		if flags.ProtoLegacy {
			if fd != nil && fd.IsWeak() && fd.Message().IsPlaceholder() {
				fd = nil // reset since the weak reference is not linked in
			}
		}

		// Handle unknown fields.
		if fd == nil {
			if d.opts.DiscardUnknown || messageDesc.ReservedNames().Has(name) {
				d.skipValue()
				continue
			}
			return d.newError(tok.Pos(), "unknown field: %v", tok.RawString())
		}

		// Handle fields identified by field number.
		if isFieldNumberName {
			// TODO: Add an option to permit parsing field numbers.
			//
			// This requires careful thought as the MarshalOptions.EmitUnknown
			// option allows formatting unknown fields as the field number and the
			// best-effort textual representation of the field value.  In that case,
			// it may not be possible to unmarshal the value from a parser that does
			// have information about the unknown field.
			return d.newError(tok.Pos(), "cannot specify field by number: %v", tok.RawString())
		}

		switch {
		case fd.IsList():
			kind := fd.Kind()
			if kind != pref.MessageKind && kind != pref.GroupKind && !tok.HasSeparator() {
				return d.syntaxError(tok.Pos(), "missing field separator :")
			}

			list := m.Mutable(fd).List()
			if err := d.unmarshalList(fd, list); err != nil {
				return err
			}

		case fd.IsMap():
			mmap := m.Mutable(fd).Map()
			if err := d.unmarshalMap(fd, mmap); err != nil {
				return err
			}

		default:
			kind := fd.Kind()
			if kind != pref.MessageKind && kind != pref.GroupKind && !tok.HasSeparator() {
				return d.syntaxError(tok.Pos(), "missing field separator :")
			}

			// If field is a oneof, check if it has already been set.
			if od := fd.ContainingOneof(); od != nil {
				idx := uint64(od.Index())
				if seenOneofs.Has(idx) {
					return d.newError(tok.Pos(), "error parsing %q, oneof %v is already set", tok.RawString(), od.FullName())
				}
				seenOneofs.Set(idx)
			}

			num := uint64(fd.Number())
			if seenNums.Has(num) {
				return d.newError(tok.Pos(), "non-repeated field %q is repeated", tok.RawString())
			}

			if err := d.unmarshalSingular(fd, m); err != nil {
				return err
			}
			seenNums.Set(num)
		}
	}

	return nil
}

// findExtension returns protoreflect.ExtensionType from the Resolver if found.
func (d decoder) findExtension(xtName pref.FullName) (pref.ExtensionType, error) {
	xt, err := d.opts.Resolver.FindExtensionByName(xtName)
	if err == nil {
		return xt, nil
	}
	return messageset.FindMessageSetExtension(d.opts.Resolver, xtName)
}

// unmarshalSingular unmarshals a non-repeated field value specified by the
// given FieldDescriptor.
func (d decoder) unmarshalSingular(fd pref.FieldDescriptor, m pref.Message) error {
	var val pref.Value
	var err error
	switch fd.Kind() {
	case pref.MessageKind, pref.GroupKind:
		val = m.NewField(fd)
		err = d.unmarshalMessage(val.Message(), true)
	default:
		val, err = d.unmarshalScalar(fd)
	}
	if err == nil {
		m.Set(fd, val)
	}
	return err
}

// unmarshalScalar unmarshals a scalar/enum protoreflect.Value specified by the
// given FieldDescriptor.
func (d decoder) unmarshalScalar(fd pref.FieldDescriptor) (pref.Value, error) {
	tok, err := d.Read()
	if err != nil {
		return pref.Value{}, err
	}

	if tok.Kind() != text.Scalar {
		return pref.Value{}, d.unexpectedTokenError(tok)
	}

	kind := fd.Kind()
	switch kind {
	case pref.BoolKind:
		if b, ok := tok.Bool(); ok {
			return pref.ValueOfBool(b), nil
		}

	case pref.Int32Kind, pref.Sint32Kind, pref.Sfixed32Kind:
		if n, ok := tok.Int32(); ok {
			return pref.ValueOfInt32(n), nil
		}

	case pref.Int64Kind, pref.Sint64Kind, pref.Sfixed64Kind:
		if n, ok := tok.Int64(); ok {
			return pref.ValueOfInt64(n), nil
		}

	case pref.Uint32Kind, pref.Fixed32Kind:
		if n, ok := tok.Uint32(); ok {
			return pref.ValueOfUint32(n), nil
		}

	case pref.Uint64Kind, pref.Fixed64Kind:
		if n, ok := tok.Uint64(); ok {
			return pref.ValueOfUint64(n), nil
		}

	case pref.FloatKind:
		if n, ok := tok.Float32(); ok {
			return pref.ValueOfFloat32(n), nil
		}

	case pref.DoubleKind:
		if n, ok := tok.Float64(); ok {
			return pref.ValueOfFloat64(n), nil
		}

	case pref.StringKind:
		if s, ok := tok.String(); ok {
			if utf8.ValidString(s) {
				return pref.ValueOfString(s), nil
			}
			return pref.Value{}, d.newError(tok.Pos(), "contains invalid UTF-8")
		}

	case pref.BytesKind:
		if b, ok := tok.String(); ok {
			return pref.ValueOfBytes([]byte(b)), nil
		}

	case pref.EnumKind:
		if lit, ok := tok.Enum(); ok {
			// Lookup EnumNumber based on name.
			if enumVal := fd.Enum().Values().ByName(pref.Name(lit)); enumVal != nil {
				return pref.ValueOfEnum(enumVal.Number()), nil
			}
		}
		if num, ok := tok.Int32(); ok {
			return pref.ValueOfEnum(pref.EnumNumber(num)), nil
		}

	default:
		panic(fmt.Sprintf("invalid scalar kind %v", kind))
	}

	return pref.Value{}, d.newError(tok.Pos(), "invalid value for %v type: %v", kind, tok.RawString())
}

// unmarshalList unmarshals into given protoreflect.List. A list value can
// either be in [] syntax or simply just a single scalar/message value.
func (d decoder) unmarshalList(fd pref.FieldDescriptor, list pref.List) error {
	tok, err := d.Peek()
	if err != nil {
		return err
	}

	switch fd.Kind() {
	case pref.MessageKind, pref.GroupKind:
		switch tok.Kind() {
		case text.ListOpen:
			d.Read()
			for {
				tok, err := d.Peek()
				if err != nil {
					return err
				}

				switch tok.Kind() {
				case text.ListClose:
					d.Read()
					return nil
				case text.MessageOpen:
					pval := list.NewElement()
					if err := d.unmarshalMessage(pval.Message(), true); err != nil {
						return err
					}
					list.Append(pval)
				default:
					return d.unexpectedTokenError(tok)
				}
			}

		case text.MessageOpen:
			pval := list.NewElement()
			if err := d.unmarshalMessage(pval.Message(), true); err != nil {
				return err
			}
			list.Append(pval)
			return nil
		}

	default:
		switch tok.Kind() {
		case text.ListOpen:
			d.Read()
			for {
				tok, err := d.Peek()
				if err != nil {
					return err
				}

				switch tok.Kind() {
				case text.ListClose:
					d.Read()
					return nil
				case text.Scalar:
					pval, err := d.unmarshalScalar(fd)
					if err != nil {
						return err
					}
					list.Append(pval)
				default:
					return d.unexpectedTokenError(tok)
				}
			}

		case text.Scalar:
			pval, err := d.unmarshalScalar(fd)
			if err != nil {
				return err
			}
			list.Append(pval)
			return nil
		}
	}

	return d.unexpectedTokenError(tok)
}

// unmarshalMap unmarshals into given protoreflect.Map. A map value is a
// textproto message containing {key: <kvalue>, value: <mvalue>}.
func (d decoder) unmarshalMap(fd pref.FieldDescriptor, mmap pref.Map) error {
	// Determine ahead whether map entry is a scalar type or a message type in
	// order to call the appropriate unmarshalMapValue func inside
	// unmarshalMapEntry.
	var unmarshalMapValue func() (pref.Value, error)
	switch fd.MapValue().Kind() {
	case pref.MessageKind, pref.GroupKind:
		unmarshalMapValue = func() (pref.Value, error) {
			pval := mmap.NewValue()
			if err := d.unmarshalMessage(pval.Message(), true); err != nil {
				return pref.Value{}, err
			}
			return pval, nil
		}
	default:
		unmarshalMapValue = func() (pref.Value, error) {
			return d.unmarshalScalar(fd.MapValue())
		}
	}

	tok, err := d.Read()
	if err != nil {
		return err
	}
	switch tok.Kind() {
	case text.MessageOpen:
		return d.unmarshalMapEntry(fd, mmap, unmarshalMapValue)

	case text.ListOpen:
		for {
			tok, err := d.Read()
			if err != nil {
				return err
			}
			switch tok.Kind() {
			case text.ListClose:
				return nil
			case text.MessageOpen:
				if err := d.unmarshalMapEntry(fd, mmap, unmarshalMapValue); err != nil {
					return err
				}
			default:
				return d.unexpectedTokenError(tok)
			}
		}

	default:
		return d.unexpectedTokenError(tok)
	}
}

// unmarshalMap unmarshals into given protoreflect.Map. A map value is a
// textproto message containing {key: <kvalue>, value: <mvalue>}.
func (d decoder) unmarshalMapEntry(fd pref.FieldDescriptor, mmap pref.Map, unmarshalMapValue func() (pref.Value, error)) error {
	var key pref.MapKey
	var pval pref.Value
Loop:
	for {
		// Read field name.
		tok, err := d.Read()
		if err != nil {
			return err
		}
		switch tok.Kind() {
		case text.Name:
			if tok.NameKind() != text.IdentName {
				if !d.opts.DiscardUnknown {
					return d.newError(tok.Pos(), "unknown map entry field %q", tok.RawString())
				}
				d.skipValue()
				continue Loop
			}
			// Continue below.
		case text.MessageClose:
			break Loop
		default:
			return d.unexpectedTokenError(tok)
		}

		name := tok.IdentName()
		switch name {
		case "key":
			if !tok.HasSeparator() {
				return d.syntaxError(tok.Pos(), "missing field separator :")
			}
			if key.IsValid() {
				return d.newError(tok.Pos(), `map entry "key" cannot be repeated`)
			}
			val, err := d.unmarshalScalar(fd.MapKey())
			if err != nil {
				return err
			}
			key = val.MapKey()

		case "value":
			if kind := fd.MapValue().Kind(); (kind != pref.MessageKind) && (kind != pref.GroupKind) {
				if !tok.HasSeparator() {
					return d.syntaxError(tok.Pos(), "missing field separator :")
				}
			}
			if pval.IsValid() {
				return d.newError(tok.Pos(), `map entry "value" cannot be repeated`)
			}
			pval, err = unmarshalMapValue()
			if err != nil {
				return err
			}

		default:
			if !d.opts.DiscardUnknown {
				return d.newError(tok.Pos(), "unknown map entry field %q", name)
			}
			d.skipValue()
		}
	}

	if !key.IsValid() {
		key = fd.MapKey().Default().MapKey()
	}
	if !pval.IsValid() {
		switch fd.MapValue().Kind() {
		case pref.MessageKind, pref.GroupKind:
			// If value field is not set for message/group types, construct an
			// empty one as default.
			pval = mmap.NewValue()
		default:
			pval = fd.MapValue().Default()
		}
	}
	mmap.Set(key, pval)
	return nil
}

// unmarshalAny unmarshals an Any textproto. It can either be in expanded form
// or non-expanded form.
func (d decoder) unmarshalAny(m pref.Message, checkDelims bool) error {
	var typeURL string
	var bValue []byte

	// hasFields tracks which valid fields have been seen in the loop below in
	// order to flag an error if there are duplicates or conflicts. It may
	// contain the strings "type_url", "value" and "expanded".  The literal
	// "expanded" is used to indicate that the expanded form has been
	// encountered already.
	hasFields := map[string]bool{}

	if checkDelims {
		tok, err := d.Read()
		if err != nil {
			return err
		}

		if tok.Kind() != text.MessageOpen {
			return d.unexpectedTokenError(tok)
		}
	}

Loop:
	for {
		// Read field name. Can only have 3 possible field names, i.e. type_url,
		// value and type URL name inside [].
		tok, err := d.Read()
		if err != nil {
			return err
		}
		if typ := tok.Kind(); typ != text.Name {
			if checkDelims {
				if typ == text.MessageClose {
					break Loop
				}
			} else if typ == text.EOF {
				break Loop
			}
			return d.unexpectedTokenError(tok)
		}

		switch tok.NameKind() {
		case text.IdentName:
			// Both type_url and value fields require field separator :.
			if !tok.HasSeparator() {
				return d.syntaxError(tok.Pos(), "missing field separator :")
			}

			switch tok.IdentName() {
			case "type_url":
				if hasFields["type_url"] {
					return d.newError(tok.Pos(), "duplicate Any type_url field")
				}
				if hasFields["expanded"] {
					return d.newError(tok.Pos(), "conflict with [%s] field", typeURL)
				}
				tok, err := d.Read()
				if err != nil {
					return err
				}
				var ok bool
				typeURL, ok = tok.String()
				if !ok {
					return d.newError(tok.Pos(), "invalid Any type_url: %v", tok.RawString())
				}
				hasFields["type_url"] = true

			case "value":
				if hasFields["value"] {
					return d.newError(tok.Pos(), "duplicate Any value field")
				}
				if hasFields["expanded"] {
					return d.newError(tok.Pos(), "conflict with [%s] field", typeURL)
				}
				tok, err := d.Read()
				if err != nil {
					return err
				}
				s, ok := tok.String()
				if !ok {
					return d.newError(tok.Pos(), "invalid Any value: %v", tok.RawString())
				}
				bValue = []byte(s)
				hasFields["value"] = true

			default:
				if !d.opts.DiscardUnknown {
					return d.newError(tok.Pos(), "invalid field name %q in google.protobuf.Any message", tok.RawString())
				}
			}

		case text.TypeName:
			if hasFields["expanded"] {
				return d.newError(tok.Pos(), "cannot have more than one type")
			}
			if hasFields["type_url"] {
				return d.newError(tok.Pos(), "conflict with type_url field")
			}
			typeURL = tok.TypeName()
			var err error
			bValue, err = d.unmarshalExpandedAny(typeURL, tok.Pos())
			if err != nil {
				return err
			}
			hasFields["expanded"] = true

		default:
			if !d.opts.DiscardUnknown {
				return d.newError(tok.Pos(), "invalid field name %q in google.protobuf.Any message", tok.RawString())
			}
		}
	}

	fds := m.Descriptor().Fields()
	if len(typeURL) > 0 {
		m.Set(fds.ByNumber(fieldnum.Any_TypeUrl), pref.ValueOfString(typeURL))
	}
	if len(bValue) > 0 {
		m.Set(fds.ByNumber(fieldnum.Any_Value), pref.ValueOfBytes(bValue))
	}
	return nil
}

func (d decoder) unmarshalExpandedAny(typeURL string, pos int) ([]byte, error) {
	mt, err := d.opts.Resolver.FindMessageByURL(typeURL)
	if err != nil {
		return nil, d.newError(pos, "unable to resolve message [%v]: %v", typeURL, err)
	}
	// Create new message for the embedded message type and unmarshal the value
	// field into it.
	m := mt.New()
	if err := d.unmarshalMessage(m, true); err != nil {
		return nil, err
	}
	// Serialize the embedded message and return the resulting bytes.
	b, err := proto.MarshalOptions{
		AllowPartial:  true, // Never check required fields inside an Any.
		Deterministic: true,
	}.Marshal(m.Interface())
	if err != nil {
		return nil, d.newError(pos, "error in marshaling message into Any.value: %v", err)
	}
	return b, nil
}

// skipValue makes the decoder parse a field value in order to advance the read
// to the next field. It relies on Read returning an error if the types are not
// in valid sequence.
func (d decoder) skipValue() error {
	tok, err := d.Read()
	if err != nil {
		return err
	}
	// Only need to continue reading for messages and lists.
	switch tok.Kind() {
	case text.MessageOpen:
		return d.skipMessageValue()

	case text.ListOpen:
		for {
			tok, err := d.Read()
			if err != nil {
				return err
			}
			switch tok.Kind() {
			case text.ListClose:
				return nil
			case text.MessageOpen:
				return d.skipMessageValue()
			default:
				// Skip items. This will not validate whether skipped values are
				// of the same type or not, same behavior as C++
				// TextFormat::Parser::AllowUnknownField(true) version 3.8.0.
				if err := d.skipValue(); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// skipMessageValue makes the decoder parse and skip over all fields in a
// message. It assumes that the previous read type is MessageOpen.
func (d decoder) skipMessageValue() error {
	for {
		tok, err := d.Read()
		if err != nil {
			return err
		}
		switch tok.Kind() {
		case text.MessageClose:
			return nil
		case text.Name:
			if err := d.skipValue(); err != nil {
				return err
			}
		}
	}
}
