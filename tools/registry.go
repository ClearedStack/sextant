// Package tools holds Sextant's sub-tools and picks the one that matches a
// target. Each sub-tool lives in its own folder and is listed in
// registry.yaml.
package tools

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"

	"github.com/ClearedStack/sextant/internal/errcode"
	"github.com/ClearedStack/sextant/internal/observation"
	"github.com/ClearedStack/sextant/internal/rulefile"
	"github.com/ClearedStack/sextant/internal/subtool"
	agencypdf "github.com/ClearedStack/sextant/tools/agency-pdf"
	govportal "github.com/ClearedStack/sextant/tools/gov-portal"
	browserjob "github.com/ClearedStack/sextant/tools/on-demand-browser-job"
	saasmarketing "github.com/ClearedStack/sextant/tools/saas-marketing"
)

//go:embed registry.yaml
var registryYAML []byte

// built maps a registry name to the code that implements it. A name in
// registry.yaml with no entry here fails loading.
var built = map[string]func() (subtool.Tool, error){
	agencypdf.Name:     agencypdf.New,
	govportal.Name:     govportal.New,
	browserjob.Name:    browserjob.New,
	saasmarketing.Name: saasmarketing.New,
}

// Registry is the loaded list of sub-tools, in registry order.
type Registry struct{ tools []subtool.Tool }

// Load reads the embedded registry.yaml and every sub-tool's rule file.
func Load() (*Registry, error) { return load(registryYAML) }

func load(src []byte) (*Registry, error) {
	root, err := rulefile.Parse(src)
	if err != nil {
		return nil, err
	}
	top, err := rulefile.Map(root, "registry.yaml", "tools")
	if err != nil {
		return nil, err
	}
	entries, err := rulefile.List(top["tools"], "tools")
	if err != nil {
		return nil, err
	}
	reg := &Registry{}
	seen := map[string]bool{}
	for _, e := range entries {
		m, err := rulefile.Map(e, "tools", "name", "folder")
		if err != nil {
			return nil, err
		}
		name, _ := rulefile.String(m["name"], "tools.name")
		folder, _ := rulefile.String(m["folder"], "tools.folder")
		construct, ok := built[name]
		if !ok || seen[name] || folder != "tools/"+name {
			return nil, fmt.Errorf("tools: entry %q is unknown, repeated or in the wrong folder", name)
		}
		seen[name] = true
		tool, err := construct()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		reg.tools = append(reg.tools, tool)
	}
	return reg, nil
}

// Names lists the registered sub-tools in order.
func (r *Registry) Names() []string {
	var names []string
	for _, t := range r.tools {
		names = append(names, t.Name())
	}
	return names
}

// Observe runs the one sub-tool whose rule matches the target. If none
// matches, or more than one does, the item is paused with E_NEEDS_SUBTOOL:
// nothing here chooses between sub-tools or falls back to a default.
func (r *Registry) Observe(t subtool.Target) (*observation.Record, error) {
	if len(t.Page) > subtool.MaxPageBytes {
		return nil, errcode.New(errcode.NeedsSubtool, errcode.PageLargerThanBudget)
	}
	if len(bytes.TrimSpace(t.Page)) == 0 {
		// Nothing was saved. That is broken data, not an unfamiliar layout.
		return nil, errcode.New(errcode.Parse, errcode.EmptyPage)
	}
	var record *observation.Record
	var failure error
	matched := 0
	for _, tool := range r.tools {
		rec, err := tool.Observe(t)
		if errors.Is(err, subtool.ErrNoMatch) {
			continue
		}
		matched++
		if err != nil {
			if _, coded := errcode.From(err); !coded {
				// A sub-tool may only fail with a public code.
				err = errcode.New(errcode.Parse, errcode.RecordFailedSchema)
			}
			if failure == nil {
				failure = err
			}
			continue
		}
		record = rec
	}
	switch {
	case matched == 0:
		return nil, errcode.New(errcode.NeedsSubtool, errcode.NoMatchingSubTool)
	case matched > 1:
		return nil, errcode.New(errcode.NeedsSubtool, errcode.AmbiguousSubTool)
	case failure != nil:
		return nil, failure
	}
	return record, nil
}
