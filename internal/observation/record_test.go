package observation

import "testing"

func valid() *Record {
	return &Record{
		ItemID: "PA-0001", SourceURL: "https://calc.example-agency.gov/x", SubTool: "gov-portal",
		ObservedOn:         "2026-01-15",
		CoreJob:            Known("date_calculation", Stated),
		Inputs:             Inputs{Fields: []InputField{{Kind: "date", Required: true}}, Tier: Seen},
		Outputs:            Outputs{Fields: []OutputField{}, Tier: Unknown},
		LoginRequirement:   NotKnown(),
		Tracking:           Tracking{ExternalHosts: []string{"a.example.net", "b.example.net"}, Tier: Seen},
		LicenseOrCopyright: Known("public_domain", Stated),
		PriceModel:         NotKnown(),
		CitedPublicRules:   CitedRules{Citations: []string{"FAR 52.212-1", "NIST SP 800-171"}, Tier: Seen},
		OfflineBehavior:    Known("server_round_trip", Seen),
	}
}

func TestValidateAcceptsAGoodRecord(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRefusesBadRecords(t *testing.T) {
	bad := map[string]func(*Record){
		"item id":              func(r *Record) { r.ItemID = "PA-1" },
		"http source":          func(r *Record) { r.SourceURL = "http://calc.example-agency.gov/" },
		"credential in source": func(r *Record) { r.SourceURL = "https://u:p@calc.example-agency.gov/" },
		"fragment in source":   func(r *Record) { r.SourceURL = "https://calc.example-agency.gov/#a" },
		"sub-tool":             func(r *Record) { r.SubTool = "Gov Portal" },
		"impossible date":      func(r *Record) { r.ObservedOn = "2026-02-30" },
		"date with time":       func(r *Record) { r.ObservedOn = "2026-01-15T00:00:00Z" },
		"free text as job":     func(r *Record) { r.CoreJob = Known("Works out a due date", Stated) },
		"unknown with value":   func(r *Record) { r.PriceModel.Value = r.CoreJob.Value },
		"known without value":  func(r *Record) { r.CoreJob.Value = nil },
		"made-up tier":         func(r *Record) { r.CoreJob.Tier = "Probably" },
		"wrong vocabulary":     func(r *Record) { r.PriceModel = Known("public_domain", Stated) },
		"input kind":           func(r *Record) { r.Inputs.Fields[0].Kind = "Issue date" },
		"null inputs":          func(r *Record) { r.Inputs = Inputs{Tier: Unknown} },
		"empty but seen":       func(r *Record) { r.Outputs.Tier = Seen },
		"filled but unknown":   func(r *Record) { r.Inputs.Tier = Unknown },
		"stated list":          func(r *Record) { r.Inputs.Tier = Stated },
		"output kind":          func(r *Record) { r.Outputs = Outputs{Fields: []OutputField{{Kind: "html"}}, Tier: Seen} },
		"unsorted hosts":       func(r *Record) { r.Tracking.ExternalHosts = []string{"b.example.net", "a.example.net"} },
		"repeated host":        func(r *Record) { r.Tracking.ExternalHosts = []string{"a.example.net", "a.example.net"} },
		"host with markup":     func(r *Record) { r.Tracking.ExternalHosts = []string{"<script>"} },
		"null hosts":           func(r *Record) { r.Tracking.ExternalHosts = nil },
		"stated tracking":      func(r *Record) { r.Tracking.Tier = Stated },
		"prose as citation":    func(r *Record) { r.CitedPublicRules.Citations = []string{"see the rule, section <b>4</b>"} },
		"unsorted citations":   func(r *Record) { r.CitedPublicRules.Citations = []string{"NIST SP 800-171", "FAR 52.212-1"} },
	}
	for name, breakIt := range bad {
		r := valid()
		breakIt(r)
		if err := r.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
