// Package robots implements a deterministic RFC 9309 robots.txt evaluator for
// Sextant's passive public fetcher.
package robots

import (
	"bytes"
	"strings"
)

// AgentToken is Sextant's product token in robots.txt and User-Agent headers.
const AgentToken = "sextant"

type rule struct {
	allow   bool
	pattern string
	weight  int
}

// Group holds the Allow and Disallow rules that apply to Sextant.
type Group struct {
	rules []rule
}

// Parse reads a robots.txt body (capped to 512 KiB per RFC 9309) and selects
// the specific "sextant" group if present, or the wildcard "*" group otherwise.
func Parse(raw []byte) Group {
	if len(raw) > 512<<10 {
		raw = raw[:512<<10]
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))

	var specific []rule
	var wildcard []rule
	hasSpecific := false

	var agents []string
	seenRuleInBlock := false

	for _, line := range strings.Split(string(raw), "\n") {
		if idx := strings.IndexByte(line, '#'); idx >= 0 {
			line = line[:idx]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		field := strings.ToLower(strings.TrimSpace(line[:colon]))
		val := strings.TrimSpace(line[colon+1:])

		switch field {
		case "user-agent":
			if seenRuleInBlock {
				agents = nil
				seenRuleInBlock = false
			}
			agents = append(agents, strings.ToLower(val))
		case "allow", "disallow":
			seenRuleInBlock = true
			if len(agents) == 0 || val == "" {
				// Empty Disallow means allow everything; ignore as no-op rule.
				continue
			}
			r := rule{
				allow:   field == "allow",
				pattern: val,
				weight:  len(val),
			}
			for _, a := range agents {
				switch a {
				case AgentToken:
					hasSpecific = true
					specific = append(specific, r)
				case "*":
					wildcard = append(wildcard, r)
				}
			}
		}
	}

	if hasSpecific {
		return Group{rules: specific}
	}
	return Group{rules: wildcard}
}

// Allowed reports whether pathWithQuery (for example "/tools/calc?x=1") may be
// fetched passively. Longest matching pattern wins; ties go to Allow per
// RFC 9309 section 2.2.2.
func (g Group) Allowed(pathWithQuery string) bool {
	if pathWithQuery == "" {
		pathWithQuery = "/"
	}
	bestWeight := -1
	bestAllow := true
	for _, r := range g.rules {
		if !matchPattern(r.pattern, pathWithQuery) {
			continue
		}
		if r.weight > bestWeight || (r.weight == bestWeight && r.allow) {
			bestWeight = r.weight
			bestAllow = r.allow
		}
	}
	return bestAllow
}

func matchPattern(pattern, target string) bool {
	anchoredEnd := strings.HasSuffix(pattern, "$")
	if anchoredEnd {
		pattern = strings.TrimSuffix(pattern, "$")
	}
	parts := strings.Split(pattern, "*")
	pos := 0
	for i, part := range parts {
		if part == "" {
			continue
		}
		if i == 0 {
			if !strings.HasPrefix(target, part) {
				return false
			}
			pos = len(part)
			continue
		}
		idx := strings.Index(target[pos:], part)
		if idx < 0 {
			return false
		}
		pos += idx + len(part)
	}
	if anchoredEnd {
		if strings.HasSuffix(pattern, "*") {
			return true
		}
		last := parts[len(parts)-1]
		return strings.HasSuffix(target, last) && len(target) >= pos
	}
	return true
}
