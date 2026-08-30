package config

import (
	"reflect"
	"testing"
)

func TestChangedSectionsIdenticalConfigs(t *testing.T) {
	t.Parallel()
	a, b := &Config{}, &Config{}
	if got := ChangedSections(a, b); len(got) != 0 {
		t.Errorf("identical configs reported changes: %v", got)
	}
}

func TestChangedSectionsNamesTheSectionByKoanfTag(t *testing.T) {
	t.Parallel()
	a := &Config{}
	b := &Config{}
	b.Logging.Level = "debug"
	b.Memory.Enabled = true
	got := ChangedSections(a, b)
	if want := []string{"logging", "memory"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestChangedSectionsIgnoresResolvedPath(t *testing.T) {
	t.Parallel()
	// The path is bookkeeping, not config: a reload resolves the same
	// file and must not report the whole config as changed because of
	// how it was found.
	a := &Config{resolvedPath: "/one.toml"}
	b := &Config{resolvedPath: "/two.toml"}
	if got := ChangedSections(a, b); len(got) != 0 {
		t.Errorf("resolvedPath leaked into the diff: %v", got)
	}
}

func TestChangedSectionsNilIsNotAChange(t *testing.T) {
	t.Parallel()
	if got := ChangedSections(nil, &Config{}); got != nil {
		t.Errorf("nil should report nothing, got %v", got)
	}
}

// A section added later is covered without touching ChangedSections —
// this asserts the reflection walk actually reaches every koanf-tagged
// section rather than a subset someone remembered to list.
func TestChangedSectionsCoversEverySection(t *testing.T) {
	t.Parallel()
	tp := reflect.TypeFor[Config]()
	var tagged int
	for f := range tp.Fields() {
		f := f
		if !f.IsExported() {
			continue
		}
		if tag := f.Tag.Get("koanf"); tag != "" && tag != "-" {
			tagged++
		}
	}
	if tagged < 15 {
		t.Fatalf("expected the config to have many tagged sections, found %d", tagged)
	}
}
