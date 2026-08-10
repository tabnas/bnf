// Copyright (c) 2025 Richard Rodger and other contributors, MIT License

package tabnaszon

// parity_test.go — cross-runtime conformance, driven by the shared
// `test/spec/*.tsv` fixtures at the repo root (see ../test/AGENTS.md), the
// same convention @tabnas/parser and @tabnas/abnf use.
//
// ts/test/parity.test.ts discovers and runs the SAME files, so the two
// implementations cannot drift without one of them going red.

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	jsonic "github.com/tabnas/jsonic/go"
)

type specRow struct {
	file     string
	lineNo   int
	input    string
	expected string
	opts     string
}

func specDir() string { return filepath.Join("..", "test", "spec") }

// specUnescape decodes the escape set used in non-JSON columns. Kept
// byte-identical to the TS loader so both runtimes feed the parser the exact
// same source text.
func specUnescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'n':
				b.WriteByte('\n')
				i++
				continue
			case 'r':
				b.WriteByte('\r')
				i++
				continue
			case 't':
				b.WriteByte('\t')
				i++
				continue
			case '\\':
				b.WriteByte('\\')
				i++
				continue
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

func loadSpec(t *testing.T, path string) []specRow {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	var rows []specRow
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		if lineNo == 1 {
			continue // header naming the columns
		}
		// Strip the CR of a CRLF line: the TS loader splits on /\r?\n/ and
		// drops it, so keeping it here would feed the runtimes different bytes.
		line := strings.TrimSuffix(scanner.Text(), "\r")
		// A comment line starts with '#' and has no tab; a data row always
		// has at least one (input + expected), so '#'-leading sources still
		// work.
		if line == "" || (strings.HasPrefix(line, "#") && !strings.Contains(line, "\t")) {
			continue
		}
		cols := strings.Split(line, "\t")
		if len(cols) < 2 {
			t.Fatalf("%s:%d: expected at least 2 tab-separated columns", path, lineNo)
		}
		row := specRow{
			file:     filepath.Base(path),
			lineNo:   lineNo,
			input:    specUnescape(cols[0]),
			expected: cols[1],
		}
		if 3 <= len(cols) {
			row.opts = cols[2]
		}
		rows = append(rows, row)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(rows) == 0 {
		t.Fatalf("%s: no cases", path)
	}
	return rows
}

// specLabel is a truncated single-line rendering of the input, so a failure
// names its case readably.
func specLabel(s string) string {
	one := strings.ReplaceAll(s, "\n", " ; ")
	if 60 < len(one) {
		return one[:57] + "..."
	}
	return one
}

// jsonRound normalises through JSON so *OrderedMap, map[string]any and the
// fixture's decoded shape compare structurally.
func jsonRound(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	return out
}

func runSpecFile(t *testing.T, path string) {
	for _, row := range loadSpec(t, path) {
		t.Run(specLabel(row.input), func(t *testing.T) {
			opts := map[string]any{}
			if strings.TrimSpace(row.opts) != "" {
				if err := json.Unmarshal([]byte(row.opts), &opts); err != nil {
					t.Fatalf("%s:%d: bad opts JSON %q: %v", row.file, row.lineNo, row.opts, err)
				}
			}

			j := jsonic.Make()
			if err := j.UseDefaults(Zon, Defaults, opts); err != nil {
				t.Fatalf("plugin init: %v", err)
			}
			got, err := j.Parse(row.input)

			if strings.HasPrefix(row.expected, "ERROR") {
				want := strings.TrimPrefix(strings.TrimPrefix(row.expected, "ERROR"), ":")
				if err == nil {
					t.Fatalf("%s:%d: expected error, got %v", row.file, row.lineNo, got)
				}
				if want != "" && !strings.Contains(err.Error(), want) {
					t.Fatalf("%s:%d: expected error %q, got %q", row.file, row.lineNo, want, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("%s:%d: unexpected parse error: %v", row.file, row.lineNo, err)
			}

			var want any
			if err := json.Unmarshal([]byte(row.expected), &want); err != nil {
				t.Fatalf("%s:%d: bad expected JSON %q: %v", row.file, row.lineNo, row.expected, err)
			}
			if gotVal := jsonRound(t, got); !reflect.DeepEqual(gotVal, want) {
				t.Errorf("%s:%d:\n  got  %#v\n  want %#v", row.file, row.lineNo, gotVal, want)
			}
		})
	}
}

// TestSpec auto-discovers every fixture: adding a .tsv runs it in both
// runtimes without touching either runner.
func TestSpec(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(specDir(), "*.tsv"))
	if err != nil {
		t.Fatalf("glob spec dir: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("no spec files under %s", specDir())
	}
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) { runSpecFile(t, path) })
	}
}
