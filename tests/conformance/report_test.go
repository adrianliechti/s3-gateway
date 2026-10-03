package conformance_test

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// node is a generic XML element; flattened paths look like
// "ListBucketResult.Contents[1].Key" and "LocationConstraint.@xmlns".
type node struct {
	name     string
	attrs    []xml.Attr
	text     string
	children []*node
}

func parseXML(body []byte) (*node, error) {
	d := xml.NewDecoder(bytes.NewReader(body))
	var stack []*node
	var root *node
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			n := &node{name: t.Name.Local, attrs: t.Attr}
			if len(stack) > 0 {
				parent := stack[len(stack)-1]
				parent.children = append(parent.children, n)
			} else if root == nil {
				root = n
			} else {
				return nil, fmt.Errorf("multiple root elements")
			}
			stack = append(stack, n)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].text += string(t)
			}
		}
	}
	if root == nil {
		return nil, fmt.Errorf("empty document")
	}
	return root, nil
}
func splitSegment(s string) (string, int) {
	if i := strings.Index(s, "["); i >= 0 && strings.HasSuffix(s, "]") {
		n, _ := strconv.Atoi(s[i+1 : len(s)-1])
		return s[:i], n
	}
	return s, 0
}
func (n *node) resolve(path string) (string, bool) {
	segments := strings.Split(path, ".")
	if name, _ := splitSegment(segments[0]); name != n.name {
		return "", false
	}
	cur := n
	for _, s := range segments[1:] {
		if strings.HasPrefix(s, "@") {
			for _, a := range cur.attrs {
				if a.Name.Local == s[1:] {
					return a.Value, true
				}
			}
			return "", false
		}
		name, idx := splitSegment(s)
		var next *node
		count := 0
		for _, c := range cur.children {
			if c.name == name {
				if count == idx {
					next = c
					break
				}
				count++
			}
		}
		if next == nil {
			return "", false
		}
		cur = next
	}
	if len(cur.children) > 0 {
		return "", true
	}
	return strings.TrimSpace(cur.text), true
}
func (n *node) lookup(path string) string { v, _ := n.resolve(path); return v }
func (n *node) exists(path string) bool   { _, ok := n.resolve(path); return ok }
func (n *node) flatten(prefix string, out map[string]string, norm func(name, value string) string) {
	key := prefix
	if key == "" {
		key = n.name
	}
	for _, a := range n.attrs {
		out[key+".@"+a.Name.Local] = norm(a.Name.Local, a.Value)
	}
	if len(n.children) == 0 {
		out[key] = norm(n.name, strings.TrimSpace(n.text))
		return
	}
	counts := map[string]int{}
	for _, c := range n.children {
		i := counts[c.name]
		counts[c.name]++
		c.flatten(fmt.Sprintf("%s.%s[%d]", key, c.name, i), out, norm)
	}
}

// Values that legitimately differ between runs or implementations are
// reduced to "{present}"; bucket names become "{bucket}" tokens.
var dynamicNames = map[string]bool{"lastmodified": true, "creationdate": true, "initiated": true, "requestid": true, "hostid": true, "id": true, "displayname": true, "uploadid": true, "nextuploadidmarker": true, "deletemarkerversionid": true, "message": true, "location": true, "nextcontinuationtoken": true, "continuationtoken": true, "expiration": true, "date": true, "x-amz-request-id": true, "x-amz-id-2": true, "last-modified": true, "x-amz-expiration": true, "x-amz-abort-date": true, "x-amz-abort-rule-id": true}
var versionNames = map[string]bool{"versionid": true, "nextversionidmarker": true, "versionidmarker": true, "uploadidmarker": true, "copysourceversionid": true, "x-amz-version-id": true, "x-amz-copy-source-version-id": true}

func (h *H) normalize(name, value string) string {
	name = strings.ToLower(name)
	for i, b := range h.buckets {
		token := "{bucket}"
		if i > 0 {
			token = fmt.Sprintf("{bucket%d}", i+1)
		}
		value = strings.ReplaceAll(value, b, token)
	}
	switch {
	case dynamicNames[name] && value != "":
		return "{present}"
	case versionNames[name] && value != "" && value != "null":
		return "{present}"
	}
	return value
}

// Semantic headers are compared against a baseline; other headers (Server,
// encryption defaults, connection management) are recorded for information.
func compared(name string, status int) bool {
	if status >= 400 {
		return name == "x-amz-delete-marker" || name == "x-amz-version-id"
	}
	// x-amz-bucket-region is asserted where documented (HeadBucket); the
	// gateway adds it to every response, which is not a protocol difference.
	switch name {
	case "etag", "content-type", "content-length", "content-range", "accept-ranges", "x-amz-version-id", "x-amz-delete-marker", "x-amz-mp-parts-count", "x-amz-tagging-count", "x-amz-storage-class", "content-encoding", "cache-control", "content-disposition", "content-language", "expires", "location", "x-amz-copy-source-version-id", "x-amz-expiration", "x-amz-checksum-type", "x-amz-object-size":
		return true
	}
	return strings.HasPrefix(name, "x-amz-meta-") || strings.HasPrefix(name, "x-amz-checksum-") || strings.HasPrefix(name, "access-control-")
}

type stepRecord struct {
	Request string            `json:"request"`
	Status  int               `json:"status"`
	Code    string            `json:"code,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Extra   map[string]string `json:"extra_headers,omitempty"`
	XML     map[string]string `json:"xml,omitempty"`
	Notes   map[string]string `json:"notes,omitempty"`
}
type divergence struct {
	Scenario string `json:"scenario"`
	Step     string `json:"step"`
	Field    string `json:"field"`
	Got      string `json:"got"`
	Want     string `json:"want"`
	Ref      string `json:"ref,omitempty"`
	URL      string `json:"url,omitempty"`
	Source   string `json:"source"`
	Known    string `json:"known,omitempty"`
}
type reportFile struct {
	Target struct {
		Name     string `json:"name"`
		Endpoint string `json:"endpoint"`
		Region   string `json:"region"`
		AWS      bool   `json:"aws"`
		// Versioning is the fixed bucket's status; a baseline recorded in a
		// versioned bucket cannot inform version fields of an unversioned run.
		Versioning string `json:"versioning,omitempty"`
	} `json:"target"`
	Generated   string                            `json:"generated_utc"`
	Baseline    string                            `json:"baseline,omitempty"`
	Summary     map[string]int                    `json:"summary"`
	Divergences []divergence                      `json:"divergences"`
	StaleKnown  []string                          `json:"known_divergences_not_observed,omitempty"`
	Scenarios   map[string]map[string]*stepRecord `json:"scenarios"`
}

var registry struct {
	sync.Mutex
	report       reportFile
	baseline     *reportFile
	expectations int
}

func (h *H) describe(r request) string {
	s := r.method + " /" + r.bucket
	if r.key != "" {
		s += "/" + r.key
	}
	if len(r.query) > 0 {
		s += "?" + encodeQuery(r.query)
	}
	return h.normalize("request", s)
}
func (h *H) record(step string, r request, res *response) {
	h.t.Helper()
	rec := &stepRecord{Request: h.describe(r), Status: res.Status, Code: res.Code, Headers: map[string]string{}, Extra: map[string]string{}}
	xmlBody := res.doc != nil || strings.HasPrefix(res.Header.Get("Content-Type"), "application/xml")
	for name, vs := range res.Header {
		l := strings.ToLower(name)
		v := h.normalize(l, strings.Join(vs, ", "))
		// XML documents differ in whitespace and transfer encoding; their
		// length says nothing about protocol conformance.
		if compared(l, res.Status) && !(l == "content-length" && xmlBody) {
			rec.Headers[l] = v
		} else {
			rec.Extra[l] = v
		}
	}
	if res.doc != nil && res.Status < 400 {
		rec.XML = map[string]string{}
		res.doc.flatten("", rec.XML, h.normalize)
	}
	registry.Lock()
	if registry.report.Scenarios[h.scenario] == nil {
		registry.report.Scenarios[h.scenario] = map[string]*stepRecord{}
	}
	if _, dup := registry.report.Scenarios[h.scenario][step]; dup {
		registry.Unlock()
		h.t.Fatalf("step %q recorded twice in scenario %q", step, h.scenario)
	}
	registry.report.Scenarios[h.scenario][step] = rec
	var base *stepRecord
	if registry.baseline != nil {
		base = registry.baseline.Scenarios[h.scenario][step]
	}
	registry.Unlock()
	if base != nil {
		h.compare(step, rec, base)
	}
}
func (h *H) compare(step string, rec, base *stepRecord) {
	h.t.Helper()
	if rec.Status != base.Status {
		h.diverge(step, "status", strconv.Itoa(rec.Status), strconv.Itoa(base.Status), "", "baseline")
	}
	if rec.Code != base.Code {
		h.diverge(step, "code", rec.Code, base.Code, "", "baseline")
	}
	versionFields := registry.baseline != nil && registry.baseline.Target.Versioning != h.tg.versioning
	diffMaps := func(prefix string, got, want map[string]string) {
		keys := map[string]bool{}
		for k := range got {
			keys[k] = true
		}
		for k := range want {
			keys[k] = true
		}
		var sorted []string
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			if got[k] == want[k] || ignoredBaselineField(prefix+k, versionFields) {
				continue
			}
			// Baselines recorded with older header rules keep headers that
			// are no longer compared.
			if prefix == "header:" && (!compared(k, rec.Status) || k == "content-length" && (len(rec.XML) > 0 || len(base.XML) > 0)) {
				continue
			}
			h.diverge(step, prefix+k, got[k], want[k], "", "baseline")
		}
	}
	diffMaps("header:", rec.Headers, base.Headers)
	diffMaps("xml:", rec.XML, base.XML)
}

// note records an observation without asserting it. A baseline run (for
// example against Amazon S3) turns notes into comparisons.
func (h *H) note(res *response, name string, value any) {
	h.t.Helper()
	v := h.normalize(name, fmt.Sprint(value))
	registry.Lock()
	rec := registry.report.Scenarios[h.scenario][res.step]
	if rec == nil {
		registry.Unlock()
		h.t.Fatalf("note %q before step %q was recorded", name, res.step)
	}
	if rec.Notes == nil {
		rec.Notes = map[string]string{}
	}
	rec.Notes[name] = v
	var want string
	var compare bool
	if registry.baseline != nil {
		if b := registry.baseline.Scenarios[h.scenario][res.step]; b != nil {
			want, compare = b.Notes[name]
		}
	}
	registry.Unlock()
	if compare && want != v {
		h.diverge(res.step, "note:"+name, v, want, "", "baseline")
	}
}

// eq is an AWS-documented expectation; ref names the documentation page.
func (h *H) eq(res *response, field string, got, want any, ref string) bool {
	h.t.Helper()
	registry.Lock()
	registry.expectations++
	registry.Unlock()
	g, w := fmt.Sprint(got), fmt.Sprint(want)
	if g == w {
		return true
	}
	h.diverge(res.step, field, g, w, ref, "docs")
	return false
}
func (h *H) status(res *response, want int, ref string) bool {
	h.t.Helper()
	return h.eq(res, "status", res.Status, want, ref)
}
func (h *H) code(res *response, status int, code, ref string) bool {
	h.t.Helper()
	ok := h.status(res, status, ref)
	return h.eq(res, "code", res.Code, code, ref) && ok
}
func (h *H) headerIs(res *response, name, want, ref string) bool {
	h.t.Helper()
	return h.eq(res, "header:"+strings.ToLower(name), res.Header.Get(name), want, ref)
}
func (h *H) xmlIs(res *response, path, want, ref string) bool {
	h.t.Helper()
	return h.eq(res, "xml:"+path, res.xml(path), want, ref)
}
func (h *H) present(res *response, path string, want bool, ref string) bool {
	h.t.Helper()
	return h.eq(res, "xml:"+path+"?", res.has(path), want, ref)
}
func (h *H) bodyIs(res *response, want string, ref string) bool {
	h.t.Helper()
	return h.eq(res, "body", string(res.Body), want, ref)
}
func (h *H) diverge(step, field, got, want, ref, source string) {
	h.t.Helper()
	key := h.scenario + "/" + step + "#" + field
	reason, known := knownDivergences[key]
	if !known {
		reason, known = knownProviderDivergences[h.tg.name+"/"+key]
	}
	if !known && strings.HasPrefix(field, "note:") {
		// A note mirrors a field whose divergence is already classified.
		reason, known = knownDivergences[h.scenario+"/"+step+"#"+strings.TrimPrefix(field, "note:")]
	}
	if !known && source == "baseline" {
		reason, known = knownBaselinePattern(key, got, want)
	}
	d := divergence{Scenario: h.scenario, Step: step, Field: field, Got: got, Want: want, Ref: ref, URL: docURL(ref), Source: source}
	if known && !h.tg.aws {
		d.Known = reason
	}
	registry.Lock()
	registry.report.Divergences = append(registry.report.Divergences, d)
	registry.Unlock()
	where := ref
	if d.URL != "" {
		where = d.URL
	}
	switch {
	case h.tg.aws && source == "docs":
		h.t.Errorf("Amazon S3 contradicts the documented expectation %s: got %q, want %q (%s)", key, got, want, where)
	case known && !h.tg.aws:
		h.t.Logf("known divergence %s: got %q, want %q: %s", key, got, want, reason)
	case source == "baseline":
		h.t.Errorf("differs from baseline %s: got %q, baseline %q", key, got, want)
	default:
		h.t.Errorf("divergence %s: got %q, want %q (%s)", key, got, want, where)
	}
}

func TestMain(m *testing.M) {
	ctx := context.Background()
	tg, err := newTarget(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "conformance:", err)
		os.Exit(1)
	}
	current = tg
	registry.report.Scenarios = map[string]map[string]*stepRecord{}
	registry.report.Divergences = []divergence{}
	registry.report.Target.Name, registry.report.Target.Endpoint, registry.report.Target.Region, registry.report.Target.AWS, registry.report.Target.Versioning = tg.name, tg.endpoint, tg.region, tg.aws, tg.versioning
	if path := os.Getenv("CONFORMANCE_BASELINE"); path != "" {
		path = resolvePath(path)
		raw, err := os.ReadFile(path)
		if err == nil {
			var base reportFile
			err = json.Unmarshal(raw, &base)
			registry.baseline = &base
			registry.report.Baseline = path
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "conformance baseline:", err)
			tg.close()
			os.Exit(1)
		}
	}
	fmt.Printf("conformance target %s at %s (region %s)\n", tg.name, tg.endpoint, tg.region)
	code := m.Run()
	tg.close()
	if err := finish(); err != nil {
		fmt.Fprintln(os.Stderr, "conformance report:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// repoRoot finds the module directory; go test runs with the package as cwd.
func repoRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// resolvePath accepts paths relative to the repository root as well as to
// the current directory.
func resolvePath(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	if _, err := os.Stat(p); err == nil {
		return p
	}
	if root := repoRoot(); root != "" {
		return filepath.Join(root, p)
	}
	return p
}
func reportPath() string {
	if p := os.Getenv("CONFORMANCE_REPORT"); p != "" {
		return resolvePath(p)
	}
	if root := repoRoot(); root != "" {
		return filepath.Join(root, "results", "conformance-"+current.name+".json")
	}
	return ""
}
func finish() error {
	registry.Lock()
	defer registry.Unlock()
	r := &registry.report
	r.Generated = time.Now().UTC().Format(time.RFC3339)
	steps := 0
	for _, s := range r.Scenarios {
		steps += len(s)
	}
	known, unknown := 0, 0
	for _, d := range r.Divergences {
		if d.Known != "" {
			known++
		} else {
			unknown++
		}
	}
	r.Summary = map[string]int{"scenarios": len(r.Scenarios), "steps": steps, "expectations": registry.expectations, "divergences": len(r.Divergences), "known_divergences": known, "unknown_divergences": unknown}
	sort.Slice(r.Divergences, func(i, j int) bool {
		if r.Divergences[i].Scenario != r.Divergences[j].Scenario {
			return r.Divergences[i].Scenario < r.Divergences[j].Scenario
		}
		if r.Divergences[i].Step != r.Divergences[j].Step {
			return r.Divergences[i].Step < r.Divergences[j].Step
		}
		return r.Divergences[i].Field < r.Divergences[j].Field
	})
	fmt.Printf("conformance summary: %d scenarios, %d steps, %d expectations, %d divergences (%d known, %d unknown)\n", len(r.Scenarios), steps, registry.expectations, len(r.Divergences), known, unknown)
	for _, d := range r.Divergences {
		label := "DIVERGENCE"
		if d.Known != "" {
			label = "known     "
		}
		fmt.Printf("  %s %s/%s#%s: got %q, want %q\n", label, d.Scenario, d.Step, d.Field, d.Got, d.Want)
	}
	// Known entries that never triggered are stale once the gateway is fixed.
	triggered := map[string]bool{}
	for _, d := range r.Divergences {
		triggered[d.Scenario+"/"+d.Step+"#"+d.Field] = true
	}
	var stale []string
	for key := range knownDivergences {
		if !triggered[key] {
			stale = append(stale, key)
		}
	}
	for key := range knownProviderDivergences {
		if target, rest, ok := strings.Cut(key, "/"); ok && target == current.name && !triggered[rest] {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	r.StaleKnown = stale
	if len(stale) > 0 && !current.aws {
		fmt.Printf("conformance: %d known divergences did not occur on this target (fixed, or specific to another backend):\n", len(stale))
		for _, key := range stale {
			fmt.Printf("  unused    %s\n", key)
		}
	}
	path := reportPath()
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	fmt.Printf("conformance report written to %s\n", path)
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
