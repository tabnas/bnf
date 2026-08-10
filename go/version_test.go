// Copyright (c) 2026 tabnas, MIT License

package bnf

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The baked-in VERSION must equal the package's declared version. This is
// the CI check for version drift; it is deliberately fatal rather than
// skipped, since a version check that silently does not run is the exact
// failure mode it exists to prevent.
func TestVersionMatchesPackageJSON(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "ts", "package.json"))
	if err != nil {
		t.Fatalf("cannot read ts/package.json, so VERSION cannot be checked: %v", err)
	}
	var pkg struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		t.Fatalf("ts/package.json is not readable JSON: %v", err)
	}
	if VERSION != pkg.Version {
		t.Errorf("VERSION drift: go VERSION = %q but %s package.json = %q",
			VERSION, pkg.Name, pkg.Version)
	}
}
