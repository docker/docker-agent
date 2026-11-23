package deepcopy

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
)

var (
	ErrUnsupported = errors.New("unsupported configuration value")
	ErrCycle       = errors.New("cyclic configuration value")
	ErrLimit       = errors.New("configuration copy limit exceeded")
)

// Value isolates mutable values, preserving scalar types and duplicating shared references.
func Value[T any](source T) (T, error) {
	var zero T
	w := walker{active: make(map[reference]bool)}
	copied, err := w.copy(reflect.ValueOf(&source).Elem(), "$", 0)
	if err != nil {
		return zero, err
	}
	reflect.ValueOf(&zero).Elem().Set(copied)
	return zero, nil
}

type reference struct {
	typ reflect.Type
	ptr uintptr
	len int
}

type walker struct {
	active map[reference]bool
	nodes  int
}

func (w *walker) copy(v reflect.Value, path string, depth int) (reflect.Value, error) {
	w.nodes++
	if depth > 128 || w.nodes > 100_000 {
		return reflect.Value{}, fmt.Errorf("%w at %s", ErrLimit, path)
	}
	kind := v.Kind()
	if kind == reflect.Func || kind == reflect.Chan || kind == reflect.UnsafePointer {
		return reflect.Value{}, fmt.Errorf("%w at %s: %s", ErrUnsupported, path, v.Type())
	}
	if kind == reflect.Pointer || kind == reflect.Map || kind == reflect.Slice {
		if v.IsNil() {
			return reflect.Zero(v.Type()), nil
		}
		ref := reference{typ: v.Type(), ptr: v.Pointer()}
		if kind == reflect.Slice {
			ref.len = v.Len()
		}
		if w.active[ref] {
			return reflect.Value{}, fmt.Errorf("%w at %s", ErrCycle, path)
		}
		w.active[ref] = true
		defer delete(w.active, ref)
	}
	out := reflect.New(v.Type()).Elem()
	switch kind {
	case reflect.Interface, reflect.Pointer:
		if v.IsNil() {
			return out, nil
		}
		child, err := w.copy(v.Elem(), path, depth+1)
		if err != nil {
			return reflect.Value{}, err
		}
		if kind == reflect.Pointer {
			out.Set(reflect.New(v.Type().Elem()))
			out.Elem().Set(child)
		} else {
			out.Set(child)
		}
	case reflect.Map:
		if v.Len() > (100_000-w.nodes)/2 {
			return reflect.Value{}, fmt.Errorf("%w at %s", ErrLimit, path)
		}
		out.Set(reflect.MakeMapWithSize(v.Type(), v.Len()))
		iter := v.MapRange()
		for iter.Next() {
			key, err := w.copy(iter.Key(), path+"[key]", depth+1)
			if err != nil {
				return reflect.Value{}, err
			}
			child, err := w.copy(iter.Value(), fmt.Sprintf("%s[%v]", path, iter.Key()), depth+1)
			if err != nil {
				return reflect.Value{}, err
			}
			out.SetMapIndex(key, child)
		}
	case reflect.Slice, reflect.Array:
		if v.Len() > 100_000-w.nodes {
			return reflect.Value{}, fmt.Errorf("%w at %s", ErrLimit, path)
		}
		if kind == reflect.Slice {
			out.Set(reflect.MakeSlice(v.Type(), v.Len(), v.Len()))
		}
		for i := range v.Len() {
			child, err := w.copy(v.Index(i), fmt.Sprintf("%s[%d]", path, i), depth+1)
			if err != nil {
				return reflect.Value{}, err
			}
			out.Index(i).Set(child)
		}
	case reflect.Struct:
		out.Set(v)
		for i := range v.NumField() {
			field := v.Type().Field(i)
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if name == "" || name == "-" {
				name = field.Name
			}
			fieldPath := path + "." + name
			if !field.IsExported() {
				if containsReferences(field.Type) {
					return reflect.Value{}, fmt.Errorf("%w at %s: unexported reference", ErrUnsupported, fieldPath)
				}
				continue
			}
			child, err := w.copy(v.Field(i), fieldPath, depth+1)
			if err != nil {
				return reflect.Value{}, err
			}
			out.Field(i).Set(child)
		}
	default:
		out.Set(v)
	}
	return out, nil
}

func containsReferences(typ reflect.Type) bool {
	switch typ.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Interface, reflect.Func, reflect.Chan, reflect.UnsafePointer:
		return true
	case reflect.Array:
		return containsReferences(typ.Elem())
	case reflect.Struct:
		for field := range typ.Fields() {
			if containsReferences(field.Type) {
				return true
			}
		}
	}
	return false
}
