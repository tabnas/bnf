// Copyright (c) 2026 tabnas, MIT License

package bnf

// rules_test.go — the schema grammar-rules, held to the shared fixtures in
// test/spec/rules.tsv, which ts/test/rules.test.js and
// rs/tests/rules_test.rs run too: each tree is the schema's or refused
// with the code its row names. A tree is read as ordered objects, so the
// members' order is held as a front end's tree holds it.

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	tabnas "github.com/tabnas/parser/go"
)

// orderedJSON reads a JSON value with its objects as *tabnas.OrderedMap.
func orderedJSON(t *testing.T, text string) any {
	t.Helper()
	if strings.HasPrefix(strings.TrimSpace(text), "{") {
		om := tabnas.NewOrderedMap()
		if err := json.Unmarshal([]byte(text), om); err != nil {
			t.Fatal(err)
		}
		return om
	}
	var out any
	dec := json.NewDecoder(bytes.NewReader([]byte(text)))
	if err := dec.Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRulesSpec(t *testing.T) {
	data, err := os.ReadFile("../test/spec/rules.tsv")
	if err != nil {
		t.Fatal(err)
	}
	rows := 0
	for i, line := range strings.Split(string(data), "\n")[1:] {
		if line == "" || (strings.HasPrefix(line, "#") && !strings.Contains(line, "\t")) {
			continue
		}
		cells := strings.SplitN(line, "\t", 2)
		input, expected := cells[0], cells[1]
		rows++
		err := CheckRules(orderedJSON(t, input))
		if expected == "OK" {
			if err != nil {
				t.Errorf("rules.tsv:%d: %v", i+2, err)
			}
			continue
		}
		var re *RulesError
		if !errors.As(err, &re) {
			t.Errorf("rules.tsv:%d: want %s, got %v", i+2, expected, err)
			continue
		}
		if "ERROR:"+re.Code != expected {
			t.Errorf("rules.tsv:%d: want %s, got %s (%v)", i+2, expected, re.Code, err)
		}
	}
	if rows != 35 {
		t.Errorf("rules.tsv holds %d rows, not 35", rows)
	}
}
