package testsupport

import (
	"reflect"
	"regexp"
)

// Field-level rules (#144): what a role may not be shown is asserted on the
// value the operation hands it, not on a page that happens to render it. A
// template that stops printing a field is not a permission; a struct that
// arrives without it is.

// Handed returns the names of v's exported fields, among names, that hold
// anything but their zero value. v is a struct or a pointer to one; a field
// that is not there is reported as handed, since a misspelt name would
// otherwise pass for one that was withheld.
func Handed(v any, names ...string) []string {
	rv := reflect.Indirect(reflect.ValueOf(v))
	var out []string
	for _, name := range names {
		f := rv.FieldByName(name)
		if !f.IsValid() || !f.IsZero() {
			out = append(out, name)
		}
	}
	return out
}

// FieldsLike returns the exported fields of a struct type, or of the struct
// a pointer or slice holds, whose names match pattern: for a type that must
// have no such field at all -- a technician's job carrying no customer.
func FieldsLike(v any, pattern string) []string {
	re := regexp.MustCompile(pattern)
	t := reflect.TypeOf(v)
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	var out []string
	for i := 0; i < t.NumField(); i++ {
		if f := t.Field(i); f.IsExported() && re.MatchString(f.Name) {
			out = append(out, f.Name)
		}
	}
	return out
}
