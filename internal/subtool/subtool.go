// Package subtool is the small contract every sub-tool under tools/ meets.
package subtool

import (
	"errors"
	"net/url"
	"strings"

	"github.com/ClearedStack/sextant/internal/observation"
)

// MaxPageBytes is the largest page Sextant will read. It keeps one
// observation well inside the 30 MiB memory budget.
const MaxPageBytes = 2 << 20

// Target is one saved public page to observe. Everything a sub-tool may use
// is in here; a sub-tool reads no clock, no environment and no network.
type Target struct {
	ItemID     string
	URL        *url.URL
	Page       []byte
	ObservedOn string // YYYY-MM-DD, supplied by the caller
}

// SourceURL is the page address as it is written into a record: lower-case
// host, no fragment.
func (t Target) SourceURL() string {
	s := t.URL.Scheme + "://" + strings.ToLower(t.URL.Host) + t.URL.EscapedPath()
	if t.URL.RawQuery != "" {
		s += "?" + t.URL.RawQuery
	}
	return s
}

// ErrNoMatch is what a Tool returns when its matching rule does not cover
// the target. It never leaves the tools package.
var ErrNoMatch = errors.New("sub-tool does not match")

// Tool is one sub-tool.
type Tool interface {
	// Name is the sub-tool's registry name.
	Name() string
	// Observe returns a record, ErrNoMatch, or an *errcode.Error.
	Observe(Target) (*observation.Record, error)
}
