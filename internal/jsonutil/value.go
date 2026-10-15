// Package jsonutil provides accessors for decoded JSON values.
package jsonutil

import (
	"encoding/json"
	"strings"
)

func FirstNonZero(values ...int) int {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

func ParseJSONOrString(value string) any {
	if value == "" {
		return map[string]any{}
	}
	var decoded any
	if json.Unmarshal([]byte(value), &decoded) == nil {
		return decoded
	}
	return value
}

func Put(object map[string]any, key string, value any) {
	if value != nil {
		object[key] = value
	}
}

func FirstAny(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func CloneMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	return cloneMap(input)
}

// cloneMap deep-copies decoded JSON without a marshal round-trip. Only
// map[string]any and []any are copied; every other value is shared as-is, so
// callers must pass trees produced by encoding/json, not maps built from typed
// Go slices or structs, which would alias instead of being normalized.
func cloneMap(input map[string]any) map[string]any {
	output := make(map[string]any, len(input))
	for key, value := range input {
		output[key] = cloneValue(value)
	}
	return output
}

func cloneValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneMap(typed)
	case []any:
		output := make([]any, len(typed))
		for i, item := range typed {
			output[i] = cloneValue(item)
		}
		return output
	default:
		return value
	}
}

func AsSlice(value any) []any {
	if values, ok := value.([]any); ok {
		return values
	}
	if value == nil {
		return nil
	}
	return []any{value}
}

func SliceAt(object map[string]any, path ...string) []any {
	values, _ := AnyAt(object, path...).([]any)
	return values
}

func MapAt(object map[string]any, path ...string) map[string]any {
	value, _ := AnyAt(object, path...).(map[string]any)
	return value
}

func StringAt(object map[string]any, path ...string) string {
	value, _ := AnyAt(object, path...).(string)
	return value
}

func BoolAt(object map[string]any, path ...string) bool {
	value, _ := AnyAt(object, path...).(bool)
	return value
}

func IntAt(object map[string]any, path ...string) int {
	value := AnyAt(object, path...)
	switch number := value.(type) {
	case float64:
		return int(number)
	case int:
		return number
	case json.Number:
		integer, _ := number.Int64()
		return int(integer)
	default:
		return 0
	}
}

func Int64At(object map[string]any, path ...string) int64 {
	return int64(IntAt(object, path...))
}

func AnyAt(object map[string]any, path ...string) any {
	var current any = object
	for _, key := range path {
		next, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = next[key]
	}
	return current
}

func FirstString(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
