package config

import (
	"context"
	"fmt"
	"reflect"

	"github.com/docker/docker-agent/pkg/environment"
)

// ExpandConfig walks all exported string fields in a config struct (recursively)
// and applies environment.Expand to each. Struct fields tagged `expand:"false"`
// are skipped (useful for fields that are JS-only and handled by Layer 2).
//
// v must be a pointer to a struct.
func ExpandConfig(ctx context.Context, v any, env environment.Provider) error {
	return walkStrings(ctx, reflect.ValueOf(v), env)
}

func walkStrings(ctx context.Context, v reflect.Value, env environment.Provider) error {
	// Dereference pointer.
	for v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}

	switch v.Kind() {
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			field := t.Field(i)
			// Skip unexported fields.
			if !field.IsExported() {
				continue
			}
			// Skip fields explicitly opted out of Layer 1 expansion.
			if field.Tag.Get("expand") == "false" {
				continue
			}
			if err := walkStrings(ctx, v.Field(i), env); err != nil {
				return fmt.Errorf("field %s: %w", field.Name, err)
			}
		}

	case reflect.String:
		if !v.CanSet() {
			return nil
		}
		expanded, _ := environment.Expand(ctx, v.String(), env)
		v.SetString(expanded)

	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			if err := walkStrings(ctx, v.Index(i), env); err != nil {
				return fmt.Errorf("[%d]: %w", i, err)
			}
		}

	case reflect.Map:
		for _, key := range v.MapKeys() {
			elem := v.MapIndex(key)
			// Map values aren't addressable; copy, expand, set.
			if elem.Kind() == reflect.String {
				expanded, _ := environment.Expand(ctx, elem.String(), env)
				v.SetMapIndex(key, reflect.ValueOf(expanded))
			}
		}
	}

	return nil
}
