// Package rulefile reads the small, strict subset of YAML that Sextant's rule
// files are written in. The Go standard library has no YAML reader, and the
// rule files need very little, so this reads exactly that little and refuses
// everything else:
//
//   - block mappings ("key: value" and "key:" followed by an indented block)
//   - block sequences ("- value" and "- key: value" items)
//   - plain scalars and 'single-quoted' scalars (two single quotes inside mean one)
//   - blank lines and whole-line "# comments"
//
// Every scalar is a string. Tabs, flow collections, double quotes, anchors,
// tags, multi-line scalars, trailing comments and duplicate keys are errors.
// Indentation is two spaces per level.
package rulefile

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Node is a string, a []Node or a map[string]Node.
type Node any

type line struct {
	number int
	indent int
	text   string
}

var keyPattern = regexp.MustCompile(`^([a-z][a-z0-9_]*):( (.*))?$`)

// Parse reads a rule file.
func Parse(src []byte) (Node, error) {
	var lines []line
	for i, raw := range strings.Split(string(src), "\n") {
		n := i + 1
		raw = strings.TrimRight(raw, " \r")
		if strings.Contains(raw, "\t") {
			return nil, fmt.Errorf("line %d: tab character", n)
		}
		text := strings.TrimLeft(raw, " ")
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		lines = append(lines, line{number: n, indent: len(raw) - len(text), text: text})
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("rule file is empty")
	}
	p := &parser{lines: lines}
	node, err := p.block(0)
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.lines) {
		return nil, fmt.Errorf("line %d: unexpected indentation", p.lines[p.pos].number)
	}
	return node, nil
}

type parser struct {
	lines []line
	pos   int
}

func (p *parser) block(indent int) (Node, error) {
	l := p.lines[p.pos]
	if l.indent != indent {
		return nil, fmt.Errorf("line %d: expected indentation of %d spaces", l.number, indent)
	}
	if strings.HasPrefix(l.text, "- ") {
		return p.sequence(indent)
	}
	return p.mapping(indent)
}

func (p *parser) sequence(indent int) (Node, error) {
	items := []Node{}
	for p.pos < len(p.lines) && p.lines[p.pos].indent == indent {
		l := &p.lines[p.pos]
		if !strings.HasPrefix(l.text, "- ") {
			return nil, fmt.Errorf("line %d: expected a list item", l.number)
		}
		rest := strings.TrimLeft(l.text[2:], " ")
		if keyPattern.MatchString(rest) {
			// "- key: value" starts a mapping whose keys sit two columns in.
			l.indent, l.text = indent+2, rest
			m, err := p.mapping(indent + 2)
			if err != nil {
				return nil, err
			}
			items = append(items, m)
			continue
		}
		s, err := scalar(rest, l.number)
		if err != nil {
			return nil, err
		}
		items = append(items, s)
		p.pos++
	}
	if p.pos < len(p.lines) && p.lines[p.pos].indent > indent {
		return nil, fmt.Errorf("line %d: unexpected indentation", p.lines[p.pos].number)
	}
	return items, nil
}

func (p *parser) mapping(indent int) (Node, error) {
	m := map[string]Node{}
	for p.pos < len(p.lines) && p.lines[p.pos].indent == indent {
		l := p.lines[p.pos]
		parts := keyPattern.FindStringSubmatch(l.text)
		if parts == nil {
			return nil, fmt.Errorf("line %d: expected \"key: value\"", l.number)
		}
		key, value := parts[1], strings.TrimLeft(parts[3], " ")
		if _, dup := m[key]; dup {
			return nil, fmt.Errorf("line %d: key %q appears twice", l.number, key)
		}
		p.pos++
		if value != "" {
			s, err := scalar(value, l.number)
			if err != nil {
				return nil, err
			}
			m[key] = s
			continue
		}
		if p.pos >= len(p.lines) || p.lines[p.pos].indent != indent+2 {
			return nil, fmt.Errorf("line %d: key %q has no value", l.number, key)
		}
		child, err := p.block(indent + 2)
		if err != nil {
			return nil, err
		}
		m[key] = child
	}
	if p.pos < len(p.lines) && p.lines[p.pos].indent > indent {
		return nil, fmt.Errorf("line %d: unexpected indentation", p.lines[p.pos].number)
	}
	return m, nil
}

func scalar(s string, n int) (string, error) {
	if strings.HasPrefix(s, "'") {
		if len(s) < 2 || !strings.HasSuffix(s, "'") {
			return "", fmt.Errorf("line %d: quote is not closed", n)
		}
		inner := s[1 : len(s)-1]
		if strings.Contains(strings.ReplaceAll(inner, "''", ""), "'") {
			return "", fmt.Errorf("line %d: a quote inside quotes must be doubled", n)
		}
		return strings.ReplaceAll(inner, "''", "'"), nil
	}
	if strings.ContainsAny(s[:1], "\"[]{}&*!|>%@`#-?:,") || strings.Contains(s, ": ") ||
		strings.Contains(s, " #") || strings.HasSuffix(s, ":") {
		return "", fmt.Errorf("line %d: this value must be written in single quotes", n)
	}
	return s, nil
}

// The helpers below turn Nodes into typed values and refuse anything that is
// not exactly what was asked for.

// Map returns n as a mapping that has exactly the given keys.
func Map(n Node, where string, keys ...string) (map[string]Node, error) {
	m, ok := n.(map[string]Node)
	if !ok {
		return nil, fmt.Errorf("%s: expected a mapping", where)
	}
	want := map[string]bool{}
	for _, k := range keys {
		want[k] = true
		if _, ok := m[k]; !ok {
			return nil, fmt.Errorf("%s: key %q is missing", where, k)
		}
	}
	var extra []string
	for k := range m {
		if !want[k] {
			extra = append(extra, k)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return nil, fmt.Errorf("%s: unknown key %q", where, extra[0])
	}
	return m, nil
}

// String returns n as a non-empty string.
func String(n Node, where string) (string, error) {
	s, ok := n.(string)
	if !ok || s == "" {
		return "", fmt.Errorf("%s: expected a value", where)
	}
	return s, nil
}

// List returns n as a non-empty sequence.
func List(n Node, where string) ([]Node, error) {
	l, ok := n.([]Node)
	if !ok || len(l) == 0 {
		return nil, fmt.Errorf("%s: expected a list", where)
	}
	return l, nil
}

// Strings returns n as a non-empty list of distinct non-empty strings.
func Strings(n Node, where string) ([]string, error) {
	l, err := List(n, where)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(l))
	for _, item := range l {
		s, err := String(item, where)
		if err != nil {
			return nil, err
		}
		if seen[s] {
			return nil, fmt.Errorf("%s: %q appears twice", where, s)
		}
		seen[s] = true
		out = append(out, s)
	}
	return out, nil
}
