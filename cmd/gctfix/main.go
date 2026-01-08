// cmd/gctfix/main.go
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type Change struct {
	Path string
	What string
}

var (
	reAllowedIDChars = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// NodeKind drives ID prefixes.
type NodeKind int

const (
	KindTemplate NodeKind = iota
	KindTrait
	KindSkill
)

func main() {
	var (
		outPath     string
		inPlace     bool
		strict      bool
		rewriteIDs  bool
		quiet       bool
		showSummary bool
	)

	flag.StringVar(&outPath, "o", "", "output .gct path (default: stdout unless -w)")
	flag.BoolVar(&inPlace, "w", false, "write in-place (overwrites input file)")
	flag.BoolVar(&strict, "strict", false, "strict mode: do not modify; only validate and exit non-zero on issues")
	flag.BoolVar(&rewriteIDs, "rewrite-ids", true, "rewrite IDs deterministically (recommended). In strict mode this is ignored.")
	flag.BoolVar(&quiet, "q", false, "quiet (less logging)")
	flag.BoolVar(&showSummary, "summary", true, "print a summary of fixes/warnings to stderr")
	flag.Usage = func() {
		_, _ = fmt.Fprintf(os.Stderr, "Usage: %s [flags] <input.gct>\n\nFlags:\n", filepath.Base(os.Args[0]))
		flag.PrintDefaults()
		_, _ = fmt.Fprintln(os.Stderr, "\nExamples:")
		_, _ = fmt.Fprintln(os.Stderr, "  gctfix in.gct > out.gct")
		_, _ = fmt.Fprintln(os.Stderr, "  gctfix -o out.gct in.gct")
		_, _ = fmt.Fprintln(os.Stderr, "  gctfix -w in.gct               # overwrite in-place")
		_, _ = fmt.Fprintln(os.Stderr, "  gctfix -strict in.gct          # validate only (no changes)")
	}
	flag.Parse()

	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	inPath := flag.Arg(0)

	b, err := os.ReadFile(inPath)
	must(err, "read input")

	var top any
	must(json.Unmarshal(b, &top), "parse JSON")
	root, ok := top.(map[string]any)
	if !ok {
		fail("top-level JSON must be an object")
	}

	changes := make([]Change, 0, 128)
	warnings := make([]Change, 0, 128)

	// --- Validate/normalize top-level ---
	ensureNumber(root, "version", 5, &changes, "$")
	ensureArray(root, "traits", &changes, "$")
	ensureArray(root, "skills", &changes, "$")
	ensureArrayDefault(root, "spells", &changes, "$")
	ensureArrayDefault(root, "notes", &changes, "$")

	// --- Normalize node trees: keys, children, reference, etc. ---
	traits := asArray(root["traits"])
	skills := asArray(root["skills"])

	// For deterministic IDs we need a stable "template path seed".
	seedName := ""
	if len(traits) > 0 {
		if n := asObject(traits[0]); n != nil {
			seedName = asString(n["name"])
		}
	}
	if seedName == "" && len(skills) > 0 {
		if n := asObject(skills[0]); n != nil {
			seedName = asString(n["name"])
		}
	}
	if seedName == "" {
		seedName = "Template"
	}

	// Normalize each tree first (structure/schema), then IDs.
	normalizeNodeArray(traits, "traits", KindTrait, &changes, &warnings)
	normalizeNodeArray(skills, "skills", KindSkill, &changes, &warnings)

	// Put normalized arrays back (normalizeNodeArray mutates in place, but be explicit).
	root["traits"] = traits
	root["skills"] = skills

	// --- Validate/fix IDs ---
	issues := make([]string, 0, 64)

	// Template ID: validate then optionally rewrite.
	rawTemplateID := asString(root["id"])
	templateIDValid := isValidID(rawTemplateID, 'B')
	if rawTemplateID == "" {
		issues = append(issues, `missing top-level "id"`)
	}
	if !templateIDValid {
		issues = append(issues, fmt.Sprintf(`invalid template id: %q`, rawTemplateID))
	}

	// Collect IDs and detect duplicates / invalid node IDs.
	used := map[string]string{} // id -> first path
	dupes := []string{}

	// In strict mode, we only validate and fail.
	if strict {
		validateIDsStrict(root, seedName, &issues, used, &dupes)
		if len(issues) > 0 || len(dupes) > 0 {
			if !quiet {
				for _, s := range issues {
					_, _ = fmt.Fprintln(os.Stderr, "ERROR:", s)
				}
				for _, d := range dupes {
					_, _ = fmt.Fprintln(os.Stderr, "ERROR:", d)
				}
			}
			os.Exit(1)
		}
		if showSummary && !quiet {
			_, _ = fmt.Fprintln(os.Stderr, "[gctfix] OK (strict): no issues found")
		}
		os.Exit(0)
	}

	// Fix mode:
	// Deterministically rewrite template id and all node ids, ensuring uniqueness.
	if rewriteIDs {
		newTemplateID := deterministicID(KindTemplate, true, "template:"+seedName, used)
		if rawTemplateID != newTemplateID {
			changes = append(changes, Change{Path: "$.id", What: fmt.Sprintf("rewrite template id %q -> %q", rawTemplateID, newTemplateID)})
		}
		root["id"] = newTemplateID
		used[newTemplateID] = "$.id"

		rewriteTreeIDs(traits, "traits", KindTrait, used, &changes)
		rewriteTreeIDs(skills, "skills", KindSkill, used, &changes)
	} else {
		// If not rewriting IDs, at least track duplicates and warn.
		validateIDsNoRewrite(root, seedName, &warnings)
	}

	// --- Emit result ---
	outBytes, err := json.Marshal(root)
	must(err, "marshal JSON")
	var pretty bytes.Buffer
	must(json.Indent(&pretty, outBytes, "", "  "), "indent JSON")
	pretty.WriteByte('\n')

	var out io.Writer = os.Stdout
	if inPlace {
		outPath = inPath
	}
	if outPath != "" {
		must(os.WriteFile(outPath, pretty.Bytes(), 0o644), "write output")
		out = nil
	}

	if out != nil {
		_, _ = out.Write(pretty.Bytes())
	}

	// --- Report ---
	if showSummary && !quiet {
		printSummary(os.Stderr, inPath, outPath, changes, warnings)
	}

	// Exit non-zero if we saw serious things worth attention (but we fixed most).
	// Keep it zero to be script-friendly unless you want CI-style behavior.
}

func printSummary(w io.Writer, inPath, outPath string, changes, warnings []Change) {
	if outPath == "" {
		outPath = "(stdout)"
	}
	_, _ = fmt.Fprintf(w, "[gctfix] input=%q output=%q\n", inPath, outPath)

	if len(changes) == 0 {
		_, _ = fmt.Fprintln(w, "[gctfix] changes: none")
	} else {
		_, _ = fmt.Fprintf(w, "[gctfix] changes: %d\n", len(changes))
		// Print a compact, stable list.
		sort.Slice(changes, func(i, j int) bool {
			if changes[i].Path == changes[j].Path {
				return changes[i].What < changes[j].What
			}
			return changes[i].Path < changes[j].Path
		})
		max := len(changes)
		if max > 50 {
			max = 50
		}
		for i := 0; i < max; i++ {
			_, _ = fmt.Fprintf(w, "  - %s: %s\n", changes[i].Path, changes[i].What)
		}
		if len(changes) > max {
			_, _ = fmt.Fprintf(w, "  ... (%d more)\n", len(changes)-max)
		}
	}

	if len(warnings) == 0 {
		_, _ = fmt.Fprintln(w, "[gctfix] warnings: none")
	} else {
		_, _ = fmt.Fprintf(w, "[gctfix] warnings: %d\n", len(warnings))
		sort.Slice(warnings, func(i, j int) bool {
			if warnings[i].Path == warnings[j].Path {
				return warnings[i].What < warnings[j].What
			}
			return warnings[i].Path < warnings[j].Path
		})
		max := len(warnings)
		if max > 50 {
			max = 50
		}
		for i := 0; i < max; i++ {
			_, _ = fmt.Fprintf(w, "  - %s: %s\n", warnings[i].Path, warnings[i].What)
		}
		if len(warnings) > max {
			_, _ = fmt.Fprintf(w, "  ... (%d more)\n", len(warnings)-max)
		}
	}
}

func normalizeNodeArray(arr []any, rootKey string, kind NodeKind, changes, warnings *[]Change) {
	for i := range arr {
		p := fmt.Sprintf("$.%s[%d]", rootKey, i)
		obj := asObject(arr[i])
		if obj == nil {
			// Replace non-object nodes with a placeholder container so structure remains valid.
			obj = map[string]any{
				"id":        "",
				"name":      fmt.Sprintf("(invalid %s node)", rootKey),
				"reference": "",
				"children":  []any{},
			}
			arr[i] = obj
			*changes = append(*changes, Change{Path: p, What: "replaced non-object node with placeholder object"})
		}
		normalizeNode(obj, p, kind, changes, warnings)
	}
}

// normalizeNode enforces:
// - required keys exist: id, name, reference, children
// - children is array
// - reference is string
// - rewrites nested traits/skills/spells/notes keys into children
func normalizeNode(n map[string]any, path string, kind NodeKind, changes, warnings *[]Change) {
	// Ensure name
	if _, ok := n["name"]; !ok {
		n["name"] = ""
		*changes = append(*changes, Change{Path: path + ".name", What: `added missing "name"=""`})
	} else if n["name"] == nil {
		n["name"] = ""
		*changes = append(*changes, Change{Path: path + ".name", What: `replaced null "name" with ""`})
	} else if _, ok := n["name"].(string); !ok {
		n["name"] = fmt.Sprint(n["name"])
		*changes = append(*changes, Change{Path: path + ".name", What: `coerced non-string "name" to string`})
	}

	// Ensure reference
	if _, ok := n["reference"]; !ok {
		n["reference"] = ""
		*changes = append(*changes, Change{Path: path + ".reference", What: `added missing "reference"=""`})
	} else if n["reference"] == nil {
		n["reference"] = ""
		*changes = append(*changes, Change{Path: path + ".reference", What: `replaced null "reference" with ""`})
	} else if _, ok := n["reference"].(string); !ok {
		n["reference"] = fmt.Sprint(n["reference"])
		*changes = append(*changes, Change{Path: path + ".reference", What: `coerced non-string "reference" to string`})
	}

	// Ensure children
	children := []any{}
	if v, ok := n["children"]; ok {
		switch vv := v.(type) {
		case []any:
			children = vv
		case nil:
			children = []any{}
			n["children"] = children
			*changes = append(*changes, Change{Path: path + ".children", What: `replaced null "children" with []`})
		default:
			// If it's some other type, coerce to empty.
			children = []any{}
			n["children"] = children
			*changes = append(*changes, Change{Path: path + ".children", What: `replaced non-array "children" with []`})
		}
	} else {
		children = []any{}
		n["children"] = children
		*changes = append(*changes, Change{Path: path + ".children", What: `added missing "children"=[]`})
	}

	// Rewrite forbidden nested keys into children.
	for _, k := range []string{"traits", "skills", "spells", "notes"} {
		if v, ok := n[k]; ok {
			if a, ok2 := v.([]any); ok2 && len(a) > 0 {
				children = append(children, a...)
				n["children"] = children
				*changes = append(*changes, Change{Path: path, What: fmt.Sprintf(`moved nested "%s" array into "children" (%d items)`, k, len(a))})
			} else {
				*changes = append(*changes, Change{Path: path, What: fmt.Sprintf(`removed nested "%s" key`, k)})
			}
			delete(n, k)
		}
	}

	// Ensure id key exists; we rewrite later, but keep schema consistent.
	if _, ok := n["id"]; !ok {
		n["id"] = ""
		*changes = append(*changes, Change{Path: path + ".id", What: `added missing "id"=""`})
	} else if n["id"] == nil {
		n["id"] = ""
		*changes = append(*changes, Change{Path: path + ".id", What: `replaced null "id" with ""`})
	} else if _, ok := n["id"].(string); !ok {
		n["id"] = fmt.Sprint(n["id"])
		*changes = append(*changes, Change{Path: path + ".id", What: `coerced non-string "id" to string`})
	}

	// Recurse.
	for i := range children {
		cp := fmt.Sprintf("%s.children[%d]", path, i)
		child := asObject(children[i])
		if child == nil {
			child = map[string]any{
				"id":        "",
				"name":      "(invalid child node)",
				"reference": "",
				"children":  []any{},
			}
			children[i] = child
			n["children"] = children
			*changes = append(*changes, Change{Path: cp, What: "replaced non-object child with placeholder object"})
		}
		normalizeNode(child, cp, kind, changes, warnings)
	}

	// Light warning: container nodes with empty reference are common, so do not error; just keep if you want.
	if len(children) > 0 && asString(n["reference"]) == "" {
		*warnings = append(*warnings, Change{Path: path, What: `container node has empty "reference" (OK, but check if you meant to set it)`})
	}
}

func rewriteTreeIDs(arr []any, rootKey string, kind NodeKind, used map[string]string, changes *[]Change) {
	for i := range arr {
		p := fmt.Sprintf("$.%s[%d]", rootKey, i)
		n := asObject(arr[i])
		if n == nil {
			continue
		}
		rewriteNodeID(n, p, kind, used, changes, fmt.Sprintf("%s/%d:%s", rootKey, i, asString(n["name"])))
	}
}

func rewriteNodeID(n map[string]any, jsonPath string, kind NodeKind, used map[string]string, changes *[]Change, stablePath string) {
	children := asArray(n["children"])
	isContainer := len(children) > 0

	var prefix byte
	switch kind {
	case KindTrait:
		if isContainer {
			prefix = 'T'
		} else {
			prefix = 't'
		}
	case KindSkill:
		if isContainer {
			prefix = 'S'
		} else {
			prefix = 's'
		}
	default:
		prefix = 'B'
	}

	oldID := asString(n["id"])
	newID := deterministicIDFromPrefix(prefix, stablePath, used)

	if oldID != newID {
		*changes = append(*changes, Change{Path: jsonPath + ".id", What: fmt.Sprintf("rewrite id %q -> %q", oldID, newID)})
	}
	n["id"] = newID
	used[newID] = jsonPath + ".id"

	// Recurse
	for i := range children {
		cp := fmt.Sprintf("%s.children[%d]", jsonPath, i)
		c := asObject(children[i])
		if c == nil {
			continue
		}
		childStable := fmt.Sprintf("%s/%d:%s", stablePath, i, asString(c["name"]))
		rewriteNodeID(c, cp, kind, used, changes, childStable)
	}
}

func deterministicID(kind NodeKind, isContainer bool, stablePath string, used map[string]string) string {
	var prefix byte
	switch kind {
	case KindTemplate:
		prefix = 'B'
	case KindTrait:
		if isContainer {
			prefix = 'T'
		} else {
			prefix = 't'
		}
	case KindSkill:
		if isContainer {
			prefix = 'S'
		} else {
			prefix = 's'
		}
	default:
		prefix = 'B'
	}
	return deterministicIDFromPrefix(prefix, stablePath, used)
}

func deterministicIDFromPrefix(prefix byte, stablePath string, used map[string]string) string {
	// collision handling: add #N to path and re-hash
	for attempt := 0; attempt < 1000; attempt++ {
		p := stablePath
		if attempt > 0 {
			p = fmt.Sprintf("%s#%d", stablePath, attempt)
		}
		sum := sha256.Sum256([]byte(p))
		enc := base64.RawURLEncoding.EncodeToString(sum[:])
		if len(enc) < 16 {
			// Should never happen.
			enc = enc + strings.Repeat("A", 16-len(enc))
		}
		id := string(prefix) + enc[:16] // 17 chars total
		if _, exists := used[id]; !exists {
			return id
		}
	}
	// Fallback (should never hit):
	sum := sha256.Sum256([]byte(stablePath + "#fallback"))
	enc := base64.RawURLEncoding.EncodeToString(sum[:])
	return string(prefix) + enc[:16]
}

func validateIDsStrict(root map[string]any, seedName string, issues *[]string, used map[string]string, dupes *[]string) {
	// Template id
	tid := asString(root["id"])
	if tid == "" {
		*issues = append(*issues, `top-level "id" is empty`)
	} else if !isValidID(tid, 'B') {
		*issues = append(*issues, fmt.Sprintf(`top-level "id" is invalid: %q`, tid))
	} else {
		used[tid] = "$.id"
	}

	checkTree := func(arr []any, rootKey string, kind NodeKind) {
		for i := range arr {
			p := fmt.Sprintf("$.%s[%d]", rootKey, i)
			n := asObject(arr[i])
			if n == nil {
				*issues = append(*issues, fmt.Sprintf("%s is not an object", p))
				continue
			}
			validateNodeIDStrict(n, p, kind, used, issues, dupes)
		}
	}

	checkTree(asArray(root["traits"]), "traits", KindTrait)
	checkTree(asArray(root["skills"]), "skills", KindSkill)

	_ = seedName // seedName unused here, but kept for potential future checks.
}

func validateNodeIDStrict(n map[string]any, jsonPath string, kind NodeKind, used map[string]string, issues *[]string, dupes *[]string) {
	children := asArray(n["children"])
	isContainer := len(children) > 0

	var prefix byte
	switch kind {
	case KindTrait:
		if isContainer {
			prefix = 'T'
		} else {
			prefix = 't'
		}
	case KindSkill:
		if isContainer {
			prefix = 'S'
		} else {
			prefix = 's'
		}
	default:
		prefix = 'B'
	}

	id := asString(n["id"])
	if id == "" {
		*issues = append(*issues, fmt.Sprintf("%s.id is empty", jsonPath))
	} else if !isValidID(id, prefix) {
		*issues = append(*issues, fmt.Sprintf("%s.id invalid for kind (%c): %q", jsonPath, prefix, id))
	} else {
		if prev, ok := used[id]; ok {
			*dupes = append(*dupes, fmt.Sprintf("duplicate id %q at %s (already used at %s)", id, jsonPath+".id", prev))
		} else {
			used[id] = jsonPath + ".id"
		}
	}

	for i := range children {
		cp := fmt.Sprintf("%s.children[%d]", jsonPath, i)
		c := asObject(children[i])
		if c == nil {
			*issues = append(*issues, fmt.Sprintf("%s is not an object", cp))
			continue
		}
		validateNodeIDStrict(c, cp, kind, used, issues, dupes)
	}
}

func validateIDsNoRewrite(root map[string]any, seedName string, warnings *[]Change) {
	_ = seedName
	used := map[string]string{}
	tid := asString(root["id"])
	if tid == "" || !isValidID(tid, 'B') {
		*warnings = append(*warnings, Change{Path: "$.id", What: fmt.Sprintf("template id is invalid: %q", tid)})
	} else {
		used[tid] = "$.id"
	}

	check := func(arr []any, rootKey string, kind NodeKind) {
		for i := range arr {
			p := fmt.Sprintf("$.%s[%d]", rootKey, i)
			n := asObject(arr[i])
			if n == nil {
				continue
			}
			walkValidateNoRewrite(n, p, kind, used, warnings)
		}
	}

	check(asArray(root["traits"]), "traits", KindTrait)
	check(asArray(root["skills"]), "skills", KindSkill)
}

func walkValidateNoRewrite(n map[string]any, jsonPath string, kind NodeKind, used map[string]string, warnings *[]Change) {
	children := asArray(n["children"])
	isContainer := len(children) > 0

	var prefix byte
	switch kind {
	case KindTrait:
		if isContainer {
			prefix = 'T'
		} else {
			prefix = 't'
		}
	case KindSkill:
		if isContainer {
			prefix = 'S'
		} else {
			prefix = 's'
		}
	default:
		prefix = 'B'
	}

	id := asString(n["id"])
	if id == "" || !isValidID(id, prefix) {
		*warnings = append(*warnings, Change{Path: jsonPath + ".id", What: fmt.Sprintf("invalid id for kind (%c): %q", prefix, id)})
	} else {
		if prev, ok := used[id]; ok {
			*warnings = append(*warnings, Change{Path: jsonPath + ".id", What: fmt.Sprintf("duplicate id %q (already used at %s)", id, prev)})
		} else {
			used[id] = jsonPath + ".id"
		}
	}

	for i := range children {
		cp := fmt.Sprintf("%s.children[%d]", jsonPath, i)
		c := asObject(children[i])
		if c == nil {
			continue
		}
		walkValidateNoRewrite(c, cp, kind, used, warnings)
	}
}

func isValidID(id string, requiredPrefix byte) bool {
	if len(id) != 17 {
		return false
	}
	if id[0] != requiredPrefix {
		return false
	}
	if !reAllowedIDChars.MatchString(id) {
		return false
	}
	return true
}

func ensureNumber(m map[string]any, key string, want float64, changes *[]Change, path string) {
	v, ok := m[key]
	if !ok {
		m[key] = want
		*changes = append(*changes, Change{Path: path + "." + key, What: fmt.Sprintf("added %q=%v", key, want)})
		return
	}
	switch vv := v.(type) {
	case float64:
		if vv != want {
			m[key] = want
			*changes = append(*changes, Change{Path: path + "." + key, What: fmt.Sprintf("replaced %q=%v with %v", key, vv, want)})
		}
	case json.Number:
		f, _ := vv.Float64()
		if f != want {
			m[key] = want
			*changes = append(*changes, Change{Path: path + "." + key, What: fmt.Sprintf("replaced %q=%v with %v", key, vv, want)})
		}
	default:
		m[key] = want
		*changes = append(*changes, Change{Path: path + "." + key, What: fmt.Sprintf("replaced non-number %q with %v", key, want)})
	}
}

func ensureArray(m map[string]any, key string, changes *[]Change, path string) {
	if v, ok := m[key]; ok {
		if _, ok2 := v.([]any); ok2 {
			return
		}
		m[key] = []any{}
		*changes = append(*changes, Change{Path: path + "." + key, What: fmt.Sprintf("replaced non-array %q with []", key)})
		return
	}
	m[key] = []any{}
	*changes = append(*changes, Change{Path: path + "." + key, What: fmt.Sprintf("added missing %q=[]", key)})
}

func ensureArrayDefault(m map[string]any, key string, changes *[]Change, path string) {
	if _, ok := m[key]; !ok {
		m[key] = []any{}
		*changes = append(*changes, Change{Path: path + "." + key, What: fmt.Sprintf("added missing %q=[]", key)})
		return
	}
	if v := m[key]; v == nil {
		m[key] = []any{}
		*changes = append(*changes, Change{Path: path + "." + key, What: fmt.Sprintf("replaced null %q with []", key)})
		return
	} else if _, ok := v.([]any); !ok {
		m[key] = []any{}
		*changes = append(*changes, Change{Path: path + "." + key, What: fmt.Sprintf("replaced non-array %q with []", key)})
	}
}

func asArray(v any) []any {
	if v == nil {
		return []any{}
	}
	if a, ok := v.([]any); ok {
		return a
	}
	return []any{}
}

func asObject(v any) map[string]any {
	if v == nil {
		return nil
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func must(err error, context string) {
	if err != nil {
		fail("%s: %v", context, err)
	}
}

func fail(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, "gctfix: "+format+"\n", args...)
	os.Exit(1)
}

