package markup

import (
	"reflect"
	"testing"

	"github.com/ClearedStack/sextant/internal/errcode"
)

func TestTokenize(t *testing.T) {
	src := "\xef\xbb\xbf<!DOCTYPE html><HTML><!-- <form> --><Title>A &amp; B</Title>" +
		"<script>if (a < b) { x = \"<form>\"; }</SCRIPT>" +
		"<p CLASS=one data-x='a > b' hidden>1 &lt; 2 < 3</p><input type=date required/><br/>"
	got, err := Tokenize([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	want := []Token{
		{Kind: Start, Name: "html"},
		{Kind: Start, Name: "title"},
		{Kind: Text, Text: "A & B", Raw: true},
		{Kind: End, Name: "title"},
		{Kind: Start, Name: "script"},
		{Kind: Text, Text: "if (a < b) { x = \"<form>\"; }", Raw: true},
		{Kind: End, Name: "script"},
		{Kind: Start, Name: "p", Attrs: map[string]string{"class": "one", "data-x": "a > b", "hidden": ""}},
		{Kind: Text, Text: "1 < 2 <"}, // a lone "<" is text
		{Kind: Text, Text: " 3"},
		{Kind: End, Name: "p"},
		{Kind: Start, Name: "input", Attrs: map[string]string{"type": "date", "required": ""}},
		{Kind: Start, Name: "br"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
	if !got[11].Has("required") || got[11].Has("name") {
		t.Error("Has is wrong")
	}
}

func TestTokenizeReportsBrokenPages(t *testing.T) {
	cases := map[string]struct {
		src    string
		reason errcode.Reason
	}{
		"empty":            {"  \n", errcode.EmptyPage},
		"not utf-8":        {"<p>\xff</p>", errcode.InvalidText},
		"nul byte":         {"<p>\x00</p>", errcode.InvalidText},
		"open comment":     {"<p><!-- never closed", errcode.TruncatedMarkup},
		"open tag":         {"<p><input type=date", errcode.TruncatedMarkup},
		"open quote":       {"<p><input name=\"a>", errcode.TruncatedMarkup},
		"open end tag":     {"<p></p", errcode.TruncatedMarkup},
		"open script":      {"<script>var a = 1;", errcode.TruncatedMarkup},
		"open declaration": {"<!DOCTYPE html", errcode.TruncatedMarkup},
	}
	for name, c := range cases {
		_, err := Tokenize([]byte(c.src))
		e, ok := errcode.From(err)
		if !ok || e.Code != errcode.Parse || e.Reason != c.reason {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

func TestLooksLikeHTML(t *testing.T) {
	yes := []string{"<!DOCTYPE html><html>", "\xef\xbb\xbf\n <!doctype HTML>", "<html lang=en>", "<HTML>"}
	no := []string{"", "%PDF-1.7", "{\"a\":1}", "<?xml version=\"1.0\"?><rss>", "<p>fragment</p>", "plain text"}
	for _, s := range yes {
		if !LooksLikeHTML([]byte(s)) {
			t.Errorf("%q should look like HTML", s)
		}
	}
	for _, s := range no {
		if LooksLikeHTML([]byte(s)) {
			t.Errorf("%q should not look like HTML", s)
		}
	}
}
