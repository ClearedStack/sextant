package robots

import "testing"

func TestParseAndAllowed(t *testing.T) {
	txt := []byte(`
# Made-up robots.txt
User-agent: *
Disallow: /private/
Allow: /private/public-calc

User-agent: sextant
Disallow: /blocked/
Disallow: /*.xls$
Allow: /blocked/open
`)
	g := Parse(txt)
	cases := []struct {
		path string
		want bool
	}{
		{"/", true},
		{"/private/secret", true}, // specific sextant group overrides wildcard
		{"/blocked/tool", false},
		{"/blocked/open/calc", true},
		{"/files/rates.xls", false},
		{"/files/rates.xls?v=1", true},
	}
	for _, c := range cases {
		if got := g.Allowed(c.path); got != c.want {
			t.Errorf("Allowed(%q) = %v, want %v", c.path, got, c.want)
		}
	}

	wild := Parse([]byte("User-agent: *\nDisallow: /\n"))
	if wild.Allowed("/anything") {
		t.Fatal("wildcard Disallow: / should block /anything")
	}
	emptyDisallow := Parse([]byte("User-agent: *\nDisallow:\n"))
	if !emptyDisallow.Allowed("/anything") {
		t.Fatal("empty Disallow should allow /anything")
	}
}
