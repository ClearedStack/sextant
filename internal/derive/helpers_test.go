package derive_test

import (
	"github.com/ClearedStack/sextant/internal/derive"
	"github.com/ClearedStack/sextant/internal/observation"
)

func derivedDefault(rec *observation.Record) (*derive.Result, error) {
	return derive.Default().Derive(rec)
}
