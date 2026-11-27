package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// A size in the override replaces its built-in entry whole, so an override that only means to make a size a dispatch
// size must repeat the rest of the entry as it is: a stale copy would undo a later change to the built-in size (#53's
// on-demand fallback and #56's gp3 settings were nearly undone that way, Nov 27).
func TestTheOverridesSizesAreTheirBuiltInEntriesPlusDispatch(t *testing.T) {
	read := func(path string) map[string]map[string]any {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var c struct {
			Sizes map[string]map[string]any `json:"sizes"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		return c.Sizes
	}
	builtIn, override := read("config.json"), read(filepath.Join("deploy", "override.json"))
	for name, entry := range override {
		rest := map[string]any{}
		for k, v := range entry {
			if k != "dispatch" {
				rest[k] = v
			}
		}
		if !reflect.DeepEqual(rest, builtIn[name]) {
			t.Errorf("%s: the override's entry, without dispatch, is %v; the built-in entry is %v", name, rest, builtIn[name])
		}
	}
}
