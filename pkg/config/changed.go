package config

import (
	"reflect"
	"sort"
	"strings"
)

// ChangedSections reports the koanf names of the top-level sections
// that differ between two configs.
//
// Derived by reflection rather than a hand-written list of sections.
// A list would be correct on the day it was written and quietly wrong
// after the next section is added — and the failure is silent, because
// a section nobody remembered to add simply never reports a change.
// Reflection makes a new section covered by existing.
//
// Sections, not leaf keys: this exists to answer "does this edit need a
// restart", and that question is answered at the granularity of the
// subsystem that consumes the section. A caller that can hot-apply part
// of a section compares that part itself.
func ChangedSections(a, b *Config) []string {
	if a == nil || b == nil {
		return nil
	}
	va, vb := reflect.ValueOf(*a), reflect.ValueOf(*b)
	t := va.Type()
	out := make([]string, 0, t.NumField())
	for i := range t.NumField() {
		f := t.Field(i)
		// Unexported fields are internal bookkeeping, not config;
		// resolvedPath in particular differs by construction and
		// reflect cannot read it anyway.
		if !f.IsExported() {
			continue
		}
		name := strings.TrimSpace(strings.Split(f.Tag.Get("koanf"), ",")[0])
		if name == "" || name == "-" {
			continue
		}
		if !reflect.DeepEqual(va.Field(i).Interface(), vb.Field(i).Interface()) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
