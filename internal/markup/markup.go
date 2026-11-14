// Package markup splits an HTML page into a flat run of tokens. It is a
// reader written for Sextant's extractors, not a browser: it builds no tree
// and repairs nothing. A page that ends inside a tag, a comment, a quoted
// attribute or a script is reported as broken rather than patched up.
package markup

import (
	"bytes"
	"html"
	"strings"
	"unicode/utf8"

	"github.com/ClearedStack/sextant/internal/errcode"
)

// Kind says what a Token is.
type Kind int

const (
	// Text is character data between tags, with entities decoded.
	Text Kind = iota
	// Start is an opening tag.
	Start
	// End is a closing tag.
	End
)

// Token is one piece of a page.
type Token struct {
	Kind  Kind
	Name  string            // lower-case tag name; empty for Text
	Attrs map[string]string // lower-case names; nil when there are none
	Text  string            // for Text only
	// Raw is true for the text inside script, style, title and textarea,
	// which is never markup.
	Raw bool
}

// Has reports whether the tag carries the attribute at all.
func (t Token) Has(name string) bool { _, ok := t.Attrs[name]; return ok }

// rawText elements hold text that must not be read as markup.
var rawText = map[string]bool{"script": true, "style": true, "title": true, "textarea": true}

// LooksLikeHTML reports whether a page starts the way an HTML document does.
func LooksLikeHTML(page []byte) bool {
	p := bytes.TrimPrefix(page, []byte("\xef\xbb\xbf"))
	p = bytes.TrimLeft(p, " \t\r\n")
	if len(p) > 16 {
		p = p[:16]
	}
	lower := strings.ToLower(string(p))
	return strings.HasPrefix(lower, "<!doctype html") || strings.HasPrefix(lower, "<html")
}

// Tokenize reads a whole page into a slice. Walk does the same work without
// holding the tokens, and is what the extractors use.
func Tokenize(page []byte) ([]Token, error) {
	var out []Token
	err := Walk(page, func(t Token) error { out = append(out, t); return nil })
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Walk reads a page and hands each token to visit, in order. It stops at the
// first error from visit and returns it. Every failure of its own is an
// E_PARSE error. Tokens are not kept, so memory use does not grow with the
// number of tags.
func Walk(page []byte, visit func(Token) error) error {
	page = bytes.TrimPrefix(page, []byte("\xef\xbb\xbf"))
	if len(bytes.TrimSpace(page)) == 0 {
		return errcode.New(errcode.Parse, errcode.EmptyPage)
	}
	if !utf8.Valid(page) || bytes.IndexByte(page, 0) >= 0 {
		return errcode.New(errcode.Parse, errcode.InvalidText)
	}
	s := string(page)
	truncated := errcode.New(errcode.Parse, errcode.TruncatedMarkup)
	var failed error
	emit := func(t Token) {
		if failed == nil {
			failed = visit(t)
		}
	}
	text := func(t string, raw bool) {
		if t != "" {
			if !raw {
				t = html.UnescapeString(t)
			}
			emit(Token{Kind: Text, Text: t, Raw: raw})
		}
	}
	i := 0
	for i < len(s) && failed == nil {
		lt := strings.IndexByte(s[i:], '<')
		if lt < 0 {
			text(s[i:], false)
			break
		}
		lt += i
		rest := s[lt:]
		switch {
		case strings.HasPrefix(rest, "<!--"):
			text(s[i:lt], false)
			end := strings.Index(rest[4:], "-->")
			if end < 0 {
				return truncated
			}
			i = lt + 4 + end + 3
		case strings.HasPrefix(rest, "<!"), strings.HasPrefix(rest, "<?"):
			text(s[i:lt], false)
			end := strings.IndexByte(rest, '>')
			if end < 0 {
				return truncated
			}
			i = lt + end + 1
		case len(rest) > 2 && rest[1] == '/' && isLetter(rest[2]):
			text(s[i:lt], false)
			end := strings.IndexByte(rest, '>')
			if end < 0 {
				return truncated
			}
			name := rest[2:end]
			if sp := strings.IndexAny(name, " \t\r\n/"); sp >= 0 {
				name = name[:sp]
			}
			emit(Token{Kind: End, Name: strings.ToLower(name)})
			i = lt + end + 1
		case len(rest) > 1 && isLetter(rest[1]):
			text(s[i:lt], false)
			tok, n, ok := startTag(rest)
			if !ok {
				return truncated
			}
			emit(tok)
			i = lt + n
			if rawText[tok.Name] {
				closer := "</" + tok.Name
				end := indexFold(s[i:], closer)
				if end < 0 {
					return truncated
				}
				body := s[i : i+end]
				if tok.Name == "title" || tok.Name == "textarea" {
					body = html.UnescapeString(body)
				}
				text(body, true)
				i += end // the closing tag is read by the next turn of the loop
			}
		default:
			// A lone "<" is ordinary text.
			text(s[i:lt+1], false)
			i = lt + 1
		}
	}
	return failed
}

// startTag reads "<name attr=value ...>" from the front of s and returns the
// token and how many bytes it used.
func startTag(s string) (Token, int, bool) {
	i := 1
	for i < len(s) && !isSpace(s[i]) && s[i] != '>' && s[i] != '/' {
		i++
	}
	tok := Token{Kind: Start, Name: strings.ToLower(s[1:i])}
	for {
		for i < len(s) && (isSpace(s[i]) || s[i] == '/') {
			i++
		}
		if i >= len(s) {
			return tok, 0, false
		}
		if s[i] == '>' {
			return tok, i + 1, true
		}
		start := i
		for i < len(s) && !isSpace(s[i]) && s[i] != '=' && s[i] != '>' && s[i] != '/' {
			i++
		}
		if i == start { // a stray "=": step over it
			i++
			continue
		}
		name := strings.ToLower(s[start:i])
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		value := ""
		if i < len(s) && s[i] == '=' {
			i++
			for i < len(s) && isSpace(s[i]) {
				i++
			}
			if i >= len(s) {
				return tok, 0, false
			}
			if q := s[i]; q == '"' || q == '\'' {
				end := strings.IndexByte(s[i+1:], q)
				if end < 0 {
					return tok, 0, false
				}
				value = s[i+1 : i+1+end]
				i += end + 2
			} else {
				start := i
				for i < len(s) && !isSpace(s[i]) && s[i] != '>' {
					i++
				}
				value = s[start:i]
			}
		}
		if tok.Attrs == nil {
			tok.Attrs = map[string]string{}
		}
		if _, dup := tok.Attrs[name]; !dup { // the first spelling wins, as in HTML
			tok.Attrs[name] = html.UnescapeString(value)
		}
	}
}

func indexFold(s, lowerNeedle string) int {
	for i := 0; i+len(lowerNeedle) <= len(s); i++ {
		if s[i] == '<' && strings.EqualFold(s[i:i+len(lowerNeedle)], lowerNeedle) {
			return i
		}
	}
	return -1
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isSpace(c byte) bool  { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' }
