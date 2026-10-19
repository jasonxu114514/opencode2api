// Package jsonutil provides accessors for decoded JSON values.
package jsonutil

import (
	"encoding/json"
	"strings"
)

// FirstNonZero returns the first value that is not zero, or 0 when all are.
func FirstNonZero(values ...int) int {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

// ParseJSONOrString decodes value as JSON, returning the decoded value when it
// parses and the string itself otherwise. An empty string yields an empty
// object rather than an empty string, so callers always get a JSON value.
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

// Put sets object[key] to value unless value is nil, in which case the key is
// left untouched. The object must not be nil.
func Put(object map[string]any, key string, value any) {
	if value != nil {
		object[key] = value
	}
}

// FirstAny returns the first value that is not nil, or nil when all are.
func FirstAny(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

// CloneMap deep-copies a decoded JSON object, keeping a nil map nil.
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

// cloneValue deep-copies the JSON containers in value; any other value is
// returned as is.
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

// AsSlice returns value as a slice: a []any unchanged, nil as nil, and any
// other value as a one-element slice. It is what lets a request field accept
// either one item or a list.
func AsSlice(value any) []any {
	if values, ok := value.([]any); ok {
		return values
	}
	if value == nil {
		return nil
	}
	return []any{value}
}

// SliceAt returns the []any at the given path, or nil when the path is missing
// or the value there is not a slice.
func SliceAt(object map[string]any, path ...string) []any {
	values, _ := AnyAt(object, path...).([]any)
	return values
}

// MapAt returns the object at the given path, or nil when the path is missing
// or the value there is not an object.
func MapAt(object map[string]any, path ...string) map[string]any {
	value, _ := AnyAt(object, path...).(map[string]any)
	return value
}

// StringAt returns the string at the given path, or "" when the path is
// missing or the value there is not a string.
func StringAt(object map[string]any, path ...string) string {
	value, _ := AnyAt(object, path...).(string)
	return value
}

// BoolAt returns the bool at the given path, or false when the path is missing
// or the value there is not a bool.
func BoolAt(object map[string]any, path ...string) bool {
	value, _ := AnyAt(object, path...).(bool)
	return value
}

// IntAt returns the number at the given path as an int, accepting float64
// (fractions are truncated), int and json.Number. Any other value, a missing
// path or an unparsable json.Number yields 0.
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

// Int64At is IntAt widened to int64.
func Int64At(object map[string]any, path ...string) int64 {
	return int64(IntAt(object, path...))
}

// AnyAt walks path through nested objects and returns the value found, or nil
// when a step is missing or the value at a step is not an object. An empty
// path returns object itself.
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

// FirstString returns the first value that is not empty after trimming space,
// trimmed, or "" when all are.
func FirstString(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
