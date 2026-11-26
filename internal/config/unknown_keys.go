package config

import (
	"reflect"
	"sort"
	"strings"
)

// UnknownKeys returns, sorted, every key in a YAML daemon config that the
// config struct does not declare (GitHub #869, reasoning control design B7).
// The decode accepts any key, so a removed or misspelt key otherwise loads
// silently and reads as a setting: chat.thinking_enabled did, for five
// months after it was removed.
//
// It reuses the two walks provenance already has: presentKeys for the file's
// dotted keys and WalkLeaves for the struct's. What it does not judge, so a
// key there is never reported:
//   - keys under a map or list leaf (model_limits.<model>.*, a list of
//     routes): the map's keys are the operator's names, not config keys;
//   - keys under a section whose type has its own UnmarshalYAML, which may
//     accept keys its struct does not declare (aliases).
func UnknownKeys(raw []byte) ([]string, error) {
	present, err := presentKeys(raw, false)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	var collect func(key string, f reflect.StructField, v reflect.Value)
	collect = func(key string, f reflect.StructField, v reflect.Value) {
		known[key] = true
		// WalkLeaves treats a doc-tagged struct section as one value (that is
		// right for the docs). Its keys are still keys, so walk into it, or a
		// misspelt key under it would read as declared.
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct && !IsLeafStruct(ft) {
			delete(known, key)
			walkLeaves(v, key, collect)
		}
	}
	WalkLeaves(reflect.ValueOf(DefaultConfig()).Elem(), collect)
	opaque := map[string]bool{}
	customSections(reflect.TypeOf(Config{}), "", opaque)

	var out []string
	for key := range present {
		if !declared(key, known, opaque) {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out, nil
}

// declared reports whether key is a struct leaf, or sits under a leaf (a map
// or list) or under a section with its own decoding.
func declared(key string, known, opaque map[string]bool) bool {
	if known[key] {
		return true
	}
	for i := strings.LastIndex(key, "."); i > 0; i = strings.LastIndex(key[:i], ".") {
		prefix := key[:i]
		if known[prefix] || opaque[prefix] {
			return true
		}
	}
	return false
}

// customSections records the dotted key of every struct section whose type
// decodes itself (an UnmarshalYAML method, either yaml.v3's or the older
// func-taking form).
func customSections(t reflect.Type, prefix string, out map[string]bool) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if !f.IsExported() || name == "" || name == "-" {
			continue
		}
		key := name
		if prefix != "" {
			key = prefix + "." + name
		}
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() != reflect.Struct {
			continue
		}
		if _, ok := reflect.PointerTo(ft).MethodByName("UnmarshalYAML"); ok {
			out[key] = true
			continue
		}
		customSections(ft, key, out)
	}
}

// UndeclaredKeys returns the config file's keys that the struct does not
// declare, recorded at load (UnknownKeys). Empty for a config built in code.
func (c *Config) UndeclaredKeys() []string {
	if c == nil {
		return nil
	}
	return c.undeclaredKeys
}
