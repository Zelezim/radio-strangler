package diff

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestJSON(t *testing.T) {
	tests := []struct {
		name      string
		legacy    string
		candidate string
		opts      Options
		want      []string
	}{
		{
			name:      "key order and PHP escaping are not differences",
			legacy:    `{"url":"https:\/\/radio.example\/live","title":"Café & Bar","n":1}`,
			candidate: `{"n":1,"title":"Café & Bar","url":"https://radio.example/live"}`,
			want:      nil,
		},
		{
			name:      "empty array vs null",
			legacy:    `{"data":[{"tags":["a"]},{"tags":[]},{"tags":[]}]}`,
			candidate: `{"data":[{"tags":["a"]},{"tags":[]},{"tags":null}]}`,
			want:      []string{"$.data[2].tags: legacy=[] go=null"},
		},
		{
			name:      "ignored field at any depth",
			legacy:    `{"generated_at":"2026-01-01T00:00:00Z","track":{"id":1,"generated_at":"a"},"list":[{"generated_at":"x"}]}`,
			candidate: `{"generated_at":"2026-01-01T00:00:09Z","track":{"id":1,"generated_at":"b"},"list":[{"generated_at":"y"}]}`,
			opts:      Options{IgnoreFields: []string{"generated_at"}},
			want:      nil,
		},
		{
			name:      "ignored field still compares its siblings",
			legacy:    `{"generated_at":"a","started_at":"2026-01-01T00:00:00Z"}`,
			candidate: `{"generated_at":"b","started_at":"2026-01-01T00:00:00.000Z"}`,
			opts:      Options{IgnoreFields: []string{"generated_at"}},
			want:      []string{`$.started_at: legacy="2026-01-01T00:00:00Z" go="2026-01-01T00:00:00.000Z"`},
		},
		{
			name:      "numbers compare by value",
			legacy:    `{"a":1,"b":12345678901234567890,"c":0.1}`,
			candidate: `{"a":1.0,"b":12345678901234567890,"c":1e-1}`,
			want:      nil,
		},
		{
			name:      "large integers stay exact",
			legacy:    `{"id":9007199254740993}`,
			candidate: `{"id":9007199254740992}`,
			want:      []string{"$.id: legacy=9007199254740993 go=9007199254740992"},
		},
		{
			name:      "string vs number is a type difference",
			legacy:    `{"id":"7"}`,
			candidate: `{"id":7}`,
			want:      []string{`$.id: legacy="7" go=7`},
		},
		{
			name:      "missing key and array length",
			legacy:    `{"album":null,"tags":["a","b","c"]}`,
			candidate: `{"tags":["a","x"]}`,
			want: []string{
				"$.album: legacy=null go=<missing>",
				"$.tags.length: legacy=3 go=2",
				`$.tags[1]: legacy="b" go="x"`,
			},
		},
		{
			name:      "object vs array at the root",
			legacy:    `{"data":[]}`,
			candidate: `[]`,
			want:      []string{`$: legacy={"data":[]} go=[]`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := JSON([]byte(tt.legacy), []byte(tt.candidate), tt.opts)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("got  %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestJSONRejectsInvalidInput(t *testing.T) {
	tests := []struct{ name, legacy, candidate string }{
		{"invalid legacy", `{"a":`, `{}`},
		{"invalid candidate", `{}`, `<html>502</html>`},
		{"empty body", ``, `{}`},
		{"trailing data", `{}`, `{} {}`},
		{"trailing garbage", `{"a":1}x`, `{"a":1}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := JSON([]byte(tt.legacy), []byte(tt.candidate), Options{}); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestJSONTrailingWhitespaceIsFine(t *testing.T) {
	if _, err := JSON([]byte("{}\n"), []byte("{}  \r\n"), Options{}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestMaxDiffs(t *testing.T) {
	var legacy, candidate []string
	for i := 0; i < 50; i++ {
		legacy = append(legacy, fmt.Sprintf(`{"tags":[],"i":%d}`, i))
		candidate = append(candidate, fmt.Sprintf(`{"tags":null,"i":%d}`, i))
	}
	l := []byte("[" + strings.Join(legacy, ",") + "]")
	c := []byte("[" + strings.Join(candidate, ",") + "]")

	if got, _ := JSON(l, c, Options{}); len(got) != DefaultMaxDiffs {
		t.Errorf("default cap: got %d diffs, want %d", len(got), DefaultMaxDiffs)
	}
	if got, _ := JSON(l, c, Options{MaxDiffs: 3}); len(got) != 3 {
		t.Errorf("custom cap: got %d diffs, want 3", len(got))
	}
}

func TestLongValuesAreTruncated(t *testing.T) {
	long := strings.Repeat("x", 200)
	got, _ := JSON([]byte(`{"a":"`+long+`"}`), []byte(`{"a":"y"}`), Options{})
	if len(got) != 1 {
		t.Fatalf("got %q", got)
	}
	legacyPart := strings.TrimPrefix(strings.Split(got[0], " go=")[0], "$.a: legacy=")
	if len([]rune(legacyPart)) != maxValueLen || !strings.HasSuffix(legacyPart, "...") {
		t.Errorf("value not truncated to %d chars: %q", maxValueLen, legacyPart)
	}
}
