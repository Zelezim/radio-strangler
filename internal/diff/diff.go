// Package diff compares two JSON documents semantically and describes every difference as a
// readable path, e.g. "$.data[2].tags: legacy=[] go=null".
//
// Comparing bytes does not work across implementations: PHP escapes "/" as "\/" and non-ASCII
// as \uXXXX while Go does neither, Go escapes <, > and & while PHP does not, key order can
// differ, and 1 vs 1.0 is the same number. Only the decoded values are the contract.
package diff

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"slices"
	"strings"
)

// DefaultMaxDiffs bounds the report: one systematic bug repeated over a long list would
// otherwise produce thousands of near-identical lines.
const DefaultMaxDiffs = 20

const maxValueLen = 80

// Options tunes a comparison.
type Options struct {
	// IgnoreFields are object keys skipped at any depth (timestamps, request ids).
	IgnoreFields []string
	// MaxDiffs caps the number of differences reported; <= 0 means DefaultMaxDiffs.
	MaxDiffs int
}

// JSON returns the differences between legacy and candidate, or an error if either side is not
// a single valid JSON value.
func JSON(legacy, candidate []byte, opts Options) ([]string, error) {
	lv, err := decode(legacy)
	if err != nil {
		return nil, fmt.Errorf("legacy body: %w", err)
	}
	cv, err := decode(candidate)
	if err != nil {
		return nil, fmt.Errorf("go body: %w", err)
	}

	c := &comparer{ignore: make(map[string]bool, len(opts.IgnoreFields)), max: opts.MaxDiffs}
	if c.max <= 0 {
		c.max = DefaultMaxDiffs
	}
	for _, f := range opts.IgnoreFields {
		c.ignore[f] = true
	}
	c.compare("$", lv, cv)
	return c.diffs, nil
}

func decode(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	// Numbers stay as their literal text so large ids and decimals are compared exactly,
	// not after a lossy float64 conversion.
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	// "{} garbage" would otherwise decode fine and hide a broken response.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected data after JSON value")
	}
	return v, nil
}

// missing marks a key present on only one side, so it renders differently from an explicit null.
type missing struct{}

type comparer struct {
	ignore map[string]bool
	max    int
	diffs  []string
}

func (c *comparer) compare(path string, a, b any) {
	if len(c.diffs) >= c.max {
		return
	}
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			c.add(path, a, b)
			return
		}
		// Sorted union of keys: deterministic output and keys missing on either side are seen.
		keys := make([]string, 0, len(av)+len(bv))
		for k := range av {
			keys = append(keys, k)
		}
		for k := range bv {
			if _, dup := av[k]; !dup {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		for _, k := range keys {
			if c.ignore[k] {
				continue
			}
			x, ok := av[k]
			if !ok {
				x = missing{}
			}
			y, ok := bv[k]
			if !ok {
				y = missing{}
			}
			c.compare(path+"."+k, x, y)
		}

	case []any:
		bv, ok := b.([]any)
		if !ok {
			c.add(path, a, b)
			return
		}
		if len(av) != len(bv) {
			c.add(path+".length", len(av), len(bv))
		}
		for i := 0; i < min(len(av), len(bv)); i++ {
			c.compare(fmt.Sprintf("%s[%d]", path, i), av[i], bv[i])
		}

	case json.Number:
		bv, ok := b.(json.Number)
		if !ok || !numbersEqual(av, bv) {
			c.add(path, a, b)
		}

	default: // string, bool, nil or missing: all comparable with ==
		if a != b {
			c.add(path, a, b)
		}
	}
}

func (c *comparer) add(path string, legacy, candidate any) {
	c.diffs = append(c.diffs, fmt.Sprintf("%s: legacy=%s go=%s", path, render(legacy), render(candidate)))
}

// numbersEqual compares by value, so 1, 1.0 and 1e0 are equal and large integers stay exact.
func numbersEqual(a, b json.Number) bool {
	x, okx := new(big.Rat).SetString(string(a))
	y, oky := new(big.Rat).SetString(string(b))
	if !okx || !oky {
		return a == b
	}
	return x.Cmp(y) == 0
}

func render(v any) string {
	if _, ok := v.(missing); ok {
		return "<missing>"
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep values readable in logs and the dashboard
	if err := enc.Encode(v); err != nil {
		return fmt.Sprintf("%v", v)
	}
	return truncate(strings.TrimSuffix(buf.String(), "\n"))
}

func truncate(s string) string {
	r := []rune(s)
	if len(r) <= maxValueLen {
		return s
	}
	return string(r[:maxValueLen-3]) + "..."
}
