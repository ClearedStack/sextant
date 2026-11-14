// Package agencypdf is the agency-pdf sub-tool: downloadable federal .gov and
// .mil procurement PDFs, fillable worksheets, and instructions.
//
// It inspects PDF objects, AcroForm dictionaries, document metadata, and
// uncompressed or Flate-compressed content streams using Go's standard library
// alone, without executing embedded scripts or emitting raw document text.
package agencypdf

import (
	"bytes"
	"compress/zlib"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/ClearedStack/sextant/internal/errcode"
	"github.com/ClearedStack/sextant/internal/observation"
	"github.com/ClearedStack/sextant/internal/subtool"
)

// Tool is the agency-pdf sub-tool.
type Tool struct{ rules *rules }

// New loads the embedded match.yaml.
func New() (subtool.Tool, error) {
	r, err := parseRules(matchYAML)
	if err != nil {
		return nil, err
	}
	return &Tool{rules: r}, nil
}

// Name is the registry name.
func (*Tool) Name() string { return Name }

var (
	objHeaderRE  = regexp.MustCompile(`(?m)(?:^|\s)\d+\s+\d+\s+obj\b`)
	pageTypeRE   = regexp.MustCompile(`/Type\s*/Page\b`)
	encryptRE    = regexp.MustCompile(`/Encrypt\b`)
	ftRE         = regexp.MustCompile(`/FT\s*/([A-Za-z0-9_.-]+)`)
	ffRE         = regexp.MustCompile(`/Ff\s+(\d+)`)
	submitFormRE = regexp.MustCompile(`/S\s*/SubmitForm\b`)
	titleDictRE  = regexp.MustCompile(`/Title\s*\(([^)\\]*(?:\\.[^)\\]*)*)\)`)
)

type pdfDoc struct {
	hasPage    bool
	title      string
	text       string
	lower      string
	infoDates  []string
	inputs     []observation.InputField
	outputs    []observation.OutputField
	submitForm bool
}

// Observe matches downloadable PDFs on .gov and .mil HTTPS hosts that contain
// at least one /Type /Page object and extracts the structured record.
func (t *Tool) Observe(target subtool.Target) (*observation.Record, error) {
	r := t.rules
	host := hostOf(target.URL)
	if !r.schemes[target.URL.Scheme] || !hasSuffix(host, r.hostSuffixes) || !hasPDFPrefix(target.Page, r.pdfPrefixes) {
		return nil, subtool.ErrNoMatch
	}

	doc, err := r.readPDF(target.Page)
	if err != nil {
		return nil, err
	}
	if !doc.hasPage {
		return nil, subtool.ErrNoMatch
	}

	pubDate, err := r.extractPubDate(doc)
	if err != nil {
		return nil, err
	}

	rec := &observation.Record{
		ItemID:          target.ItemID,
		SourceURL:       target.SourceURL(),
		SubTool:         Name,
		ObservedOn:      target.ObservedOn,
		PublicationDate: &pubDate,
		Inputs:          observation.Inputs{Fields: []observation.InputField{}, Tier: observation.Unknown},
		Outputs:         observation.Outputs{Fields: []observation.OutputField{}, Tier: observation.Unknown},
		Tracking:        observation.Tracking{ExternalHosts: []string{}, Tier: observation.Seen},
	}
	if len(doc.inputs) > 0 {
		rec.Inputs = observation.Inputs{Fields: doc.inputs, Tier: observation.Seen}
	}
	if len(doc.outputs) > 0 {
		rec.Outputs = observation.Outputs{Fields: doc.outputs, Tier: observation.Seen}
	}

	headline := doc.title
	if headline == "" {
		headline = doc.lower
	}
	if rec.CoreJob, err = classify(r.coreJob, headline, errcode.AmbiguousCoreJob); err != nil {
		return nil, err
	}
	if rec.LoginRequirement, err = classify(r.login, doc.lower, errcode.AmbiguousLogin); err != nil {
		return nil, err
	}
	if rec.LicenseOrCopyright, err = classify(r.license, doc.lower, errcode.AmbiguousLicense); err != nil {
		return nil, err
	}
	if rec.PriceModel, err = classify(r.price, doc.lower, errcode.AmbiguousPriceModel); err != nil {
		return nil, err
	}
	if doc.submitForm {
		rec.OfflineBehavior = observation.Known("server_round_trip", observation.Seen)
	} else if rec.OfflineBehavior, err = classify(r.offline, doc.lower, errcode.AmbiguousOffline); err != nil {
		return nil, err
	}

	rec.CitedPublicRules = r.cite(doc.text)

	if err := rec.Validate(); err != nil {
		return nil, errcode.New(errcode.Parse, errcode.RecordFailedSchema)
	}
	return rec, nil
}

func (r *rules) readPDF(raw []byte) (*pdfDoc, error) {
	trimmed := bytes.TrimRight(raw, " \t\r\n")
	if !bytes.Contains(trimmed, []byte("%%EOF")) {
		return nil, errcode.New(errcode.Parse, errcode.CorruptedPDF)
	}
	if encryptRE.Match(raw) {
		return nil, errcode.New(errcode.NeedsSubtool, errcode.EncryptedPDF)
	}

	starts := objHeaderRE.FindAllIndex(raw, -1)
	endCount := bytes.Count(raw, []byte("endobj"))
	if len(starts) != endCount {
		return nil, errcode.New(errcode.Parse, errcode.CorruptedPDF)
	}
	if len(starts) == 0 {
		return &pdfDoc{}, nil
	}

	doc := &pdfDoc{}
	var textBuf spaced
	totalDecoded := 0

	for _, loc := range starts {
		bodyStart := loc[1]
		relEnd := bytes.Index(raw[bodyStart:], []byte("endobj"))
		if relEnd < 0 {
			return nil, errcode.New(errcode.Parse, errcode.CorruptedPDF)
		}
		objBytes := raw[bodyStart : bodyStart+relEnd]
		dictBytes, streamData, hasStream, err := splitStream(objBytes)
		if err != nil {
			return nil, err
		}

		dictStr := string(dictBytes)
		if pageTypeRE.Match(dictBytes) {
			doc.hasPage = true
		}
		if submitFormRE.Match(dictBytes) {
			doc.submitForm = true
		}
		if m := titleDictRE.FindStringSubmatch(dictStr); m != nil && doc.title == "" {
			doc.title = normalise(unescapePDFString(m[1]))
		}
		for _, key := range r.dateInfoKeys {
			if d, ok := findInfoDate(dictStr, key); ok {
				doc.infoDates = append(doc.infoDates, d)
			}
		}
		if m := ftRE.FindStringSubmatch(dictStr); m != nil {
			if err := r.collectField(dictStr, strings.ToLower(m[1]), doc); err != nil {
				return nil, err
			}
		}

		if hasStream {
			content, err := decodeStream(dictBytes, streamData)
			if err != nil {
				return nil, err
			}
			totalDecoded += len(content)
			if totalDecoded > subtool.MaxPageBytes {
				return nil, errcode.New(errcode.NeedsSubtool, errcode.PageLargerThanBudget)
			}
			extractStreamText(content, &textBuf)
		}
	}

	doc.text = textBuf.String()
	doc.lower = strings.ToLower(doc.text)
	return doc, nil
}

func splitStream(obj []byte) (dict []byte, stream []byte, hasStream bool, err error) {
	idx := bytes.Index(obj, []byte("stream"))
	for idx >= 0 {
		// Ensure "stream" is a standalone keyword followed by \n or \r\n, not "endstream".
		prevOK := idx == 0 || !isIdent(obj[idx-1])
		after := idx + len("stream")
		if prevOK && after < len(obj) && (obj[after] == '\n' || obj[after] == '\r') {
			dataStart := after + 1
			if obj[after] == '\r' && dataStart < len(obj) && obj[dataStart] == '\n' {
				dataStart++
			}
			endIdx := bytes.Index(obj[dataStart:], []byte("endstream"))
			if endIdx < 0 {
				return nil, nil, false, errcode.New(errcode.Parse, errcode.CorruptedPDF)
			}
			data := obj[dataStart : dataStart+endIdx]
			data = bytes.TrimSuffix(data, []byte("\n"))
			data = bytes.TrimSuffix(data, []byte("\r"))
			return obj[:idx], data, true, nil
		}
		next := bytes.Index(obj[after:], []byte("stream"))
		if next < 0 {
			break
		}
		idx = after + next
	}
	if bytes.Contains(obj, []byte("endstream")) {
		return nil, nil, false, errcode.New(errcode.Parse, errcode.CorruptedPDF)
	}
	return obj, nil, false, nil
}

func decodeStream(dict, data []byte) ([]byte, error) {
	if !bytes.Contains(dict, []byte("/FlateDecode")) {
		if bytes.Contains(dict, []byte("/Filter")) {
			return nil, errcode.New(errcode.NeedsSubtool, errcode.NoMatchingSubTool)
		}
		return data, nil
	}
	zr, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, errcode.New(errcode.Parse, errcode.CorruptedPDF)
	}
	defer zr.Close()
	out, err := io.ReadAll(io.LimitReader(zr, subtool.MaxPageBytes+1))
	if err != nil {
		return nil, errcode.New(errcode.Parse, errcode.CorruptedPDF)
	}
	if len(out) > subtool.MaxPageBytes {
		return nil, errcode.New(errcode.NeedsSubtool, errcode.PageLargerThanBudget)
	}
	return out, nil
}

func (r *rules) collectField(dictStr, ft string, doc *pdfDoc) error {
	flags := 0
	if m := ffRE.FindStringSubmatch(dictStr); m != nil {
		flags, _ = strconv.Atoi(m[1])
	}
	kind, ok := r.fieldKinds[ft]
	if !ok {
		return errcode.New(errcode.NeedsSubtool, errcode.UnfamiliarPDFField)
	}
	if ft == "btn" && flags&r.ignoredButtonFlag != 0 {
		return nil
	}
	if flags&r.readonlyFlag != 0 {
		doc.outputs = append(doc.outputs, observation.OutputField{Kind: r.readonlyOutputKind})
		return nil
	}
	doc.inputs = append(doc.inputs, observation.InputField{
		Kind:     kind,
		Required: flags&r.requiredFlag != 0,
	})
	return nil
}

func findInfoDate(dictStr, key string) (string, bool) {
	needle := "/" + key
	idx := strings.Index(dictStr, needle)
	if idx < 0 {
		return "", false
	}
	rest := strings.TrimLeft(dictStr[idx+len(needle):], " \t\r\n")
	if !strings.HasPrefix(rest, "(") {
		return "", false
	}
	end := strings.IndexByte(rest[1:], ')')
	if end < 0 {
		return "", false
	}
	val := strings.TrimPrefix(rest[1:1+end], "D:")
	if len(val) < 8 {
		return "", false
	}
	iso := val[0:4] + "-" + val[4:6] + "-" + val[6:8]
	if !observation.ValidDate(iso) {
		return "", false
	}
	return iso, true
}

func (r *rules) extractPubDate(doc *pdfDoc) (observation.Fact, error) {
	found := ""
	tier := observation.Unknown
	for _, d := range doc.infoDates {
		if found != "" && found != d {
			return observation.Fact{}, errcode.New(errcode.NeedsSubtool, errcode.AmbiguousPubDate)
		}
		found = d
		tier = observation.Seen
	}
	if found != "" {
		return observation.Known(found, tier), nil
	}
	for _, re := range r.datePatterns {
		for _, m := range re.FindAllStringSubmatch(doc.text, -1) {
			d := m[1]
			if !observation.ValidDate(d) {
				return observation.Fact{}, errcode.New(errcode.Parse, errcode.CorruptedPDF)
			}
			if found != "" && found != d {
				return observation.Fact{}, errcode.New(errcode.NeedsSubtool, errcode.AmbiguousPubDate)
			}
			found = d
			tier = observation.Stated
		}
	}
	if found == "" {
		return observation.NotKnown(), nil
	}
	return observation.Known(found, tier), nil
}

// extractStreamText pulls literal string operands (...) from PDF text streams.
func extractStreamText(stream []byte, out *spaced) {
	s := string(stream)
	for i := 0; i < len(s); i++ {
		if s[i] != '(' {
			continue
		}
		var b strings.Builder
		depth := 1
		i++
		for i < len(s) && depth > 0 {
			ch := s[i]
			if ch == '\\' && i+1 < len(s) {
				i++
				b.WriteByte('\\')
				b.WriteByte(s[i])
			} else if ch == '(' {
				depth++
				b.WriteByte(ch)
			} else if ch == ')' {
				depth--
				if depth > 0 {
					b.WriteByte(ch)
				}
			} else {
				b.WriteByte(ch)
			}
			i++
		}
		i--
		out.add(unescapePDFString(b.String()))
	}
}

func unescapePDFString(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case '(', ')', '\\':
			b.WriteByte(s[i])
		default:
			if s[i] >= '0' && s[i] <= '7' {
				oct := string(s[i])
				for k := 0; k < 2 && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '7'; k++ {
					i++
					oct += string(s[i])
				}
				if v, err := strconv.ParseUint(oct, 8, 8); err == nil {
					b.WriteByte(byte(v))
				}
			} else {
				b.WriteByte(s[i])
			}
		}
	}
	return b.String()
}

func classify(list []wordRule, lower string, ambiguous errcode.Reason) (observation.Fact, error) {
	found := ""
	for _, rule := range list {
		for _, phrase := range rule.phrases {
			if strings.Contains(lower, phrase) {
				if found != "" && found != rule.value {
					return observation.Fact{}, errcode.New(errcode.NeedsSubtool, ambiguous)
				}
				found = rule.value
				break
			}
		}
	}
	if found == "" {
		return observation.NotKnown(), nil
	}
	return observation.Known(found, observation.Stated), nil
}

func (r *rules) cite(text string) observation.CitedRules {
	seen := map[string]bool{}
	out := []string{}
	for _, c := range r.citations {
		for _, m := range c.pattern.FindAllStringSubmatchIndex(text, -1) {
			s := string(c.pattern.ExpandString(nil, c.write, text, m))
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	if len(out) == 0 {
		return observation.CitedRules{Citations: out, Tier: observation.Unknown}
	}
	sort.Strings(out)
	return observation.CitedRules{Citations: out, Tier: observation.Seen}
}

func hasPDFPrefix(page []byte, prefixes []string) bool {
	p := bytes.TrimLeft(page, " \t\r\n")
	for _, pref := range prefixes {
		if bytes.HasPrefix(p, []byte(pref)) {
			return true
		}
	}
	return false
}

func isIdent(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func hostOf(u *url.URL) string {
	return strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
}

func hasSuffix(host string, suffixes []string) bool {
	for _, s := range suffixes {
		if strings.HasSuffix(host, s) && len(host) > len(s) {
			return true
		}
	}
	return false
}

type spaced struct {
	b   strings.Builder
	gap bool
}

func (s *spaced) add(piece string) {
	for _, c := range piece {
		if unicode.IsSpace(c) {
			s.gap = true
			continue
		}
		if s.gap && s.b.Len() > 0 {
			s.b.WriteByte(' ')
		}
		s.gap = false
		s.b.WriteRune(c)
	}
	s.gap = true
}

func (s *spaced) String() string { return s.b.String() }

func normalise(text string) string {
	var s spaced
	s.add(text)
	return strings.ToLower(s.String())
}
