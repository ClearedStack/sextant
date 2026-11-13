package rulefile

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	src := `# comment
name: gov-portal
quoted: 'a: b ''c'' # not a comment'

nested:
  list:
    - .gov
    - '\bFAR\s+(\d+)'
  items:
    - value: one
      phrases:
        - first phrase
    - value: two
      phrases:
        - ©
`
	got, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Node{
		"name":   "gov-portal",
		"quoted": "a: b 'c' # not a comment",
		"nested": map[string]Node{
			"list": []Node{".gov", `\bFAR\s+(\d+)`},
			"items": []Node{
				map[string]Node{"value": "one", "phrases": []Node{"first phrase"}},
				map[string]Node{"value": "two", "phrases": []Node{"©"}},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
}

func TestParseRefusesWhatItDoesNotRead(t *testing.T) {
	bad := map[string]string{
		"empty":            "# only a comment\n",
		"tab":              "a:\n\t- b\n",
		"flow list":        "a: [b, c]\n",
		"flow map":         "a: {b: c}\n",
		"double quotes":    "a: \"b\"\n",
		"anchor":           "a: &x b\n",
		"block scalar":     "a: |\n  text\n",
		"trailing comment": "a: b # c\n",
		"duplicate key":    "a: b\na: c\n",
		"key no value":     "a:\nb: c\n",
		"odd indent":       "a:\n   b: c\n",
		"deeper item":      "a:\n  - b\n    - c\n",
		"open quote":       "a: 'b\n",
		"bare quote":       "a: 'b'c'\n",
		"colon in plain":   "a: b: c\n",
		"mixed block":      "a:\n  - b\n  c: d\n",
		"upper-case key":   "Key: b\n",
		"document marker":  "---\na: b\n",
	}
	for name, src := range bad {
		if node, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: accepted as %#v", name, node)
		}
	}
}

func TestTypedHelpers(t *testing.T) {
	n, _ := Parse([]byte("a: x\nb:\n  - y\n  - y\n"))
	if _, err := Map(n, "top", "a"); err == nil {
		t.Error("Map accepted an unknown key")
	}
	if _, err := Map(n, "top", "a", "b", "c"); err == nil {
		t.Error("Map accepted a missing key")
	}
	m, err := Map(n, "top", "a", "b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Strings(m["b"], "b"); err == nil {
		t.Error("Strings accepted a repeated value")
	}
	if _, err := String(m["b"], "b"); err == nil {
		t.Error("String accepted a list")
	}
}
