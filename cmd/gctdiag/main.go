package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/richardwilkes/gcs/v5/model/gurps"
)

func main() {
	var path string
	var rawOnly bool
	var maxDepth int

	flag.StringVar(&path, "file", "", "path to .gct file")
	flag.BoolVar(&rawOnly, "raw-only", false, "only parse and analyze raw JSON (do not call gurps.NewTemplateFromFile)")
	flag.IntVar(&maxDepth, "max-depth", 50, "maximum recursion depth for raw JSON walk")
	flag.Parse()

	if path == "" && flag.NArg() > 0 {
		path = flag.Arg(0)
	}
	if path == "" {
		fmt.Fprintln(os.Stderr, "usage: gctdiag path/to/template.gct")
		os.Exit(2)
	}

	abs, _ := filepath.Abs(path)
	fmt.Fprintf(os.Stderr, "[gctdiag] file=%q\n", abs)

	raw, err := os.ReadFile(path)
	if err != nil {
		fail("read", err)
	}

	printFileFacts(path, raw)

	fmt.Fprintln(os.Stderr, "\n[gctdiag] RAW JSON parse")
	root, err := parseJSON(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[gctdiag] raw json unmarshal FAILED: %v\n", err)
		os.Exit(1)
	}

	rawID := getString(root, "id")
	fmt.Fprintf(os.Stderr, "[gctdiag] raw template id=%q\n", rawID)
	fmt.Fprintf(os.Stderr, "[gctdiag] top-level keys: %s\n", joinKeys(root))

	traits := getArray(root, "traits")
	skills := getArray(root, "skills")

	fmt.Fprintf(os.Stderr, "[gctdiag] raw: traits=%d skills=%d\n", len(traits), len(skills))

	fmt.Fprintln(os.Stderr, "\n[gctdiag] RAW TRAITS tree (children)")
	rawTreeWalk(traits, "traits", 0, maxDepth, "Trait")

	fmt.Fprintln(os.Stderr, "\n[gctdiag] RAW SKILLS tree (children)")
	rawTreeWalk(skills, "skills", 0, maxDepth, "Skill")

	fmt.Fprintln(os.Stderr, "\n[gctdiag] RAW warnings")
	rawWarnings(root)

	if rawOnly {
		return
	}

	dir := os.DirFS(filepath.Dir(path))
	base := filepath.Base(path)
	t, err := gurps.NewTemplateFromFile(dir, base)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n[gctdiag] load FAILED: %v\n", err)
		printErrChain(err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr,
		"\n[gctdiag] load OK: id=%s title=%q traits=%d skills=%d spells=%d notes=%d\n",
		t.ID,
		t.PageTitle(),
		len(t.TraitList()),
		len(t.SkillList()),
		len(t.SpellList()),
		len(t.NoteList()),
	)

	loadedID := fmt.Sprint(t.ID)
        if rawID != "" && rawID != loadedID {
		fmt.Fprintf(os.Stderr, "[gctdiag] WARNING: template id was rewritten on load (raw %q -> loaded %q)\n", rawID, t.ID)
		fmt.Fprintln(os.Stderr, "[gctdiag]          This strongly suggests your generator is not emitting GCS-valid IDs.")
		fmt.Fprintln(os.Stderr, "[gctdiag]          ID repair can coincide with normalization that drops/repairs child structures.")
	}
}

func parseJSON(b []byte) (map[string]any, error) {
	// handle gzip just in case
	if len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b {
		r, err := gzip.NewReader(bytes.NewReader(b))
		if err == nil {
			defer r.Close()
			if out, rerr := io.ReadAll(r); rerr == nil {
				b = out
			}
		}
	}
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		return nil, err
	}
	return root, nil
}

func rawTreeWalk(nodes []any, path string, depth, maxDepth int, kind string) {
	if depth > maxDepth {
		return
	}
	indent := strings.Repeat("  ", depth)
	for i, n := range nodes {
		obj, ok := n.(map[string]any)
		if !ok {
			fmt.Fprintf(os.Stderr, "%s- (%s) %s[%d]: non-object %T\n", indent, kind, path, i, n)
			continue
		}
		name := getString(obj, "name")
		id := getString(obj, "id")
		ref := getString(obj, "reference")
		child := getArray(obj, "children")

		points := ""
		if calc, ok := obj["calc"].(map[string]any); ok {
			if p, ok := calc["points"]; ok {
				points = fmt.Sprintf(" points=%v", p)
			}
		}

		fmt.Fprintf(os.Stderr, "%s- %s [%s] id=%q ref=%q children=%d%s\n",
			indent, safeName(name), kind, id, ref, len(child), points)

		if len(child) > 0 {
			rawTreeWalk(child, fmt.Sprintf("%s[%d].children", path, i), depth+1, maxDepth, kind)
		}
	}
}

func rawWarnings(root map[string]any) {
	traits := getArray(root, "traits")
	skills := getArray(root, "skills")

	// 1) nodes with children but missing reference
	var badRefs []string
	walkForMissingRef(traits, "traits", &badRefs)
	walkForMissingRef(skills, "skills", &badRefs)

	for _, s := range badRefs {
		fmt.Fprintf(os.Stderr, "[gctdiag] WARNING: has children but missing/empty reference: %s\n", s)
	}

	// 2) skills that are missing reference at all (common mismatch vs stock files)
	var skillNoRef []string
	walkForMissingRefField(skills, "skills", &skillNoRef)
	for _, s := range skillNoRef {
		fmt.Fprintf(os.Stderr, "[gctdiag] WARNING: skill node missing 'reference' field entirely: %s\n", s)
	}

	if len(badRefs) == 0 && len(skillNoRef) == 0 {
		fmt.Fprintln(os.Stderr, "[gctdiag] (no obvious raw-JSON warnings found)")
	}
}

func walkForMissingRef(nodes []any, path string, out *[]string) {
	for i, n := range nodes {
		obj, ok := n.(map[string]any)
		if !ok {
			continue
		}
		child := getArray(obj, "children")
		if len(child) > 0 {
			ref := getString(obj, "reference")
			if strings.TrimSpace(ref) == "" {
				name := getString(obj, "name")
				*out = append(*out, fmt.Sprintf("%s[%d] name=%q id=%q", path, i, name, getString(obj, "id")))
			}
			walkForMissingRef(child, fmt.Sprintf("%s[%d].children", path, i), out)
		}
	}
}

func walkForMissingRefField(nodes []any, path string, out *[]string) {
	for i, n := range nodes {
		obj, ok := n.(map[string]any)
		if !ok {
			continue
		}
		_, hasRef := obj["reference"]
		if !hasRef {
			name := getString(obj, "name")
			*out = append(*out, fmt.Sprintf("%s[%d] name=%q id=%q", path, i, name, getString(obj, "id")))
		}
		child := getArray(obj, "children")
		if len(child) > 0 {
			walkForMissingRefField(child, fmt.Sprintf("%s[%d].children", path, i), out)
		}
	}
}

func getArray(m map[string]any, key string) []any {
	if v, ok := m[key]; ok {
		if a, ok := v.([]any); ok {
			return a
		}
	}
	return nil
}

func getString(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func safeName(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(no name)"
	}
	return s
}

func joinKeys(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

func printFileFacts(path string, data []byte) {
	fi, err := os.Stat(path)
	if err == nil {
		fmt.Fprintf(os.Stderr, "[gctdiag] size=%d bytes\n", fi.Size())
	}
	sum := sha256.Sum256(data)
	fmt.Fprintf(os.Stderr, "[gctdiag] sha256=%x\n", sum)

	head := data
	if len(head) > 96 {
		head = head[:96]
	}
	fmt.Fprintf(os.Stderr, "[gctdiag] sniff=%s\n", sniffFormat(data))
	fmt.Fprintf(os.Stderr, "[gctdiag] head=%q\n", printable(head))
}

func sniffFormat(b []byte) string {
	if len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b {
		return "gzip"
	}
	tb := bytes.TrimSpace(b)
	if len(tb) > 0 && (tb[0] == '{' || tb[0] == '[') {
		return "json-ish"
	}
	return "unknown"
}

func printable(b []byte) string {
	var sb strings.Builder
	for _, c := range b {
		if c >= 32 && c <= 126 {
			sb.WriteByte(c)
		} else {
			sb.WriteString(fmt.Sprintf("\\x%02x", c))
		}
	}
	return sb.String()
}

func printErrChain(err error) {
	i := 0
	for err != nil {
		fmt.Fprintf(os.Stderr, "[gctdiag] err[%d] %T: %v\n", i, err, err)
		err = errors.Unwrap(err)
		i++
	}
	fmt.Fprintf(os.Stderr, "[gctdiag] stack:\n%s\n", bytes.TrimSpace(debug.Stack()))
}

func fail(stage string, err error) {
	fmt.Fprintf(os.Stderr, "[gctdiag] %s FAILED: %v\n", stage, err)
	printErrChain(err)
	os.Exit(1)
}

