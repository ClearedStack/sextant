package derive_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ClearedStack/sextant/internal/derive"
	"github.com/ClearedStack/sextant/internal/errcode"
	"github.com/ClearedStack/sextant/internal/observation"
)

// FuzzDerive feeds arbitrary strings into the places a record holds text.
// Whatever goes in, Derive must not panic; it either returns a result that
// validates and holds none of the input, or one of Sextant's coded errors.
func FuzzDerive(f *testing.F) {
	f.Add("FAR 31.203", "rate_calculation", "free", "not_required", "t.example.net", uint8(0))
	f.Add("FAR Part 31", "amount_calculation", "paid", "required", "", uint8(1))
	f.Add("FAR 31.201-2", "eligibility_check", "free", "required", "a.b", uint8(2))
	f.Add("zzcanary secret phrase", "zzcanary", "zzcanary", "zzcanary", "zzcanary", uint8(255))
	f.Fuzz(func(t *testing.T, cite, job, price, login, host string, mode uint8) {
		rec := &observation.Record{
			ItemID: "PA-0300", SourceURL: "https://rates.example-tools.org/wrap", SubTool: "saas-marketing", ObservedOn: "2026-01-15",
			CoreJob:            fuzzFact(job, mode&1 != 0),
			Inputs:             observation.Inputs{Fields: []observation.InputField{{Kind: "number", Required: true}}, Tier: observation.Stated},
			Outputs:            observation.Outputs{Fields: []observation.OutputField{{Kind: "computed_value"}}, Tier: observation.Stated},
			LoginRequirement:   fuzzFact(login, mode&2 != 0),
			Tracking:           observation.Tracking{ExternalHosts: []string{host}, Tier: observation.Seen},
			LicenseOrCopyright: observation.Known("open_license", observation.Stated),
			PriceModel:         fuzzFact(price, mode&4 != 0),
			CitedPublicRules:   observation.CitedRules{Citations: []string{cite}, Tier: observation.Seen},
			OfflineBehavior:    observation.Known("works_offline", observation.Stated),
		}
		if mode&8 != 0 {
			rec.Tracking = observation.Tracking{ExternalHosts: []string{}, Tier: observation.Unknown}
		}
		res, err := derive.Default().Derive(rec)
		if err != nil {
			var coded *errcode.Error
			if !errors.As(err, &coded) || res != nil {
				t.Fatalf("uncoded error or result with error: %v", err)
			}
			return
		}
		if err := res.Validate(); err != nil {
			t.Fatalf("result does not validate: %v", err)
		}
		out, err := json.Marshal(res)
		if err != nil {
			t.Fatal(err)
		}
		for _, in := range []string{cite} {
			if len(in) > 3 && strings.Contains(string(out), in) {
				t.Fatalf("input text %q reached the result", in)
			}
		}
		again, _ := derive.Default().Derive(rec)
		if again2, _ := json.Marshal(again); string(again2) != string(out) {
			t.Fatal("same input, different output")
		}
	})
}

func fuzzFact(v string, unknown bool) observation.Fact {
	if unknown {
		return observation.NotKnown()
	}
	return observation.Known(v, observation.Stated)
}
