#!/usr/bin/env python3
"""
md2gctspec.py

Markdown -> GCTSPEC (strict intermediate spec) preprocessor.

Goals:
- Convert ambiguous Markdown into an explicit, typed tree:
  - traits: list[group|trait nodes]
  - skills: list[group|skill nodes]
- Extract points/reference hints
- Fail loudly (errors[]) if nodes cannot be typed confidently.

Usage:
  python3 md2gctspec.py AMP_Base_Template.md -o AMP_Base_Template.gctspec.json
  python3 md2gctspec.py AMP_Base_Template.md --yaml -o AMP_Base_Template.gctspec.yaml
"""

from __future__ import annotations

import argparse
import dataclasses
import datetime as dt
import json
import os
import re
import sys
from typing import Any, Dict, List, Optional, Tuple

# --- Regexes ---
HEADING_RE = re.compile(r"^(#{1,6})\s+(.+?)\s*$")
BULLET_RE = re.compile(r"^(\s*)([-*+])\s+(.*)$")

# points patterns:
#   "10 points", "-15 pts", "[10]", "(10 pts)", "points: 10", "pt: -5"
POINTS_PATTERNS = [
    re.compile(r"\bpoints?\s*:\s*([+-]?\d+)\b", re.IGNORECASE),
    re.compile(r"\b([+-]?\d+)\s*(?:points?|pts?|pt)\b", re.IGNORECASE),
    re.compile(r"\[([+-]?\d+)\]\s*$"),
    re.compile(r"\(([+-]?\d+)\s*(?:points?|pts?|pt)\)\s*$", re.IGNORECASE),
]

# references like B198, B65, S233, TT1: etc. (keep it permissive but not insane)
REF_RE = re.compile(r"\b([A-Z]{1,4}\d{1,4}|TT\d:)\b")

# explicit tags
TAG_RE = re.compile(r"^(TRAIT|SKILL|GROUP)\s*:\s*(.+)$", re.IGNORECASE)

# section keywords for context
SKILLS_SECTION_KEYS = ("skill", "skills")
TRAITS_SECTION_KEYS = ("trait", "traits", "advantage", "advantages", "disadvantage", "disadvantages")


@dataclasses.dataclass
class Node:
    raw: str
    line_no: int
    indent: int
    kind: str  # 'group' | 'trait' | 'skill' | 'unknown'
    name: str
    points: Optional[int]
    reference: Optional[str]
    children: List["Node"]

    # convenience metadata for debugging
    context_hint: str  # 'skills'|'traits'|'unknown'
    path_key: str      # stable-ish path for downstream deterministic IDs

    def to_spec(self) -> Dict[str, Any]:
        out: Dict[str, Any] = {
            "kind": self.kind,
            "name": self.name,
        }
        if self.points is not None:
            out["points"] = self.points
        if self.reference:
            out["reference"] = self.reference
        if self.children:
            out["children"] = [c.to_spec() for c in self.children]
        out["_src"] = {"line": self.line_no, "raw": self.raw, "path_key": self.path_key, "context": self.context_hint}
        return out


def normalize_title(s: str) -> str:
    return re.sub(r"\s+", " ", s.strip())


def detect_context_from_heading(title: str) -> str:
    t = title.strip().lower()
    if any(k in t for k in SKILLS_SECTION_KEYS):
        return "skills"
    if any(k in t for k in TRAITS_SECTION_KEYS):
        return "traits"
    return "unknown"


def extract_points(text: str) -> Tuple[Optional[int], str]:
    """Return (points, stripped_text_without_points_hint_if_obvious)."""
    for rx in POINTS_PATTERNS:
        m = rx.search(text)
        if m:
            try:
                pts = int(m.group(1))
            except Exception:
                continue
            # strip just the matched segment if it looks like a suffix
            stripped = text
            if m.start() > len(text) * 0.6:  # likely suffix
                stripped = (text[: m.start()] + text[m.end() :]).strip()
                stripped = stripped.rstrip("—-–:,;")
            return pts, stripped
    return None, text


def extract_reference(text: str) -> Tuple[Optional[str], str]:
    """Return (reference, stripped_text_without_ref_if_obvious)."""
    # Prefer refs that appear in explicit "ref:" patterns if present
    m = re.search(r"\bref\s*:\s*([A-Z]{1,4}\d{1,4}|TT\d:)\b", text, re.IGNORECASE)
    if m:
        ref = m.group(1)
        stripped = (text[: m.start()] + text[m.end() :]).strip()
        stripped = stripped.rstrip("—-–:,;")
        return ref, stripped

    # Otherwise, pick the last ref-looking token (often suffix "B198")
    refs = REF_RE.findall(text)
    if refs:
        ref = refs[-1]
        # strip if suffix-like
        idx = text.rfind(ref)
        stripped = text
        if idx > len(text) * 0.6:
            stripped = (text[:idx] + text[idx + len(ref):]).strip()
            stripped = stripped.rstrip("—-–:,;")
        return ref, stripped
    return None, text


def parse_tag(text: str) -> Tuple[Optional[str], str]:
    m = TAG_RE.match(text.strip())
    if not m:
        return None, text
    tag = m.group(1).lower()
    rest = m.group(2).strip()
    if tag == "trait":
        return "trait", rest
    if tag == "skill":
        return "skill", rest
    if tag == "group":
        return "group", rest
    return None, text


def looks_like_group(name: str, points: Optional[int], ref: Optional[str], has_children: bool) -> bool:
    # If it has children, it's almost certainly a group/container.
    if has_children:
        return True
    # Titles like "Advantages", "Disadvantages", "Core Skills", etc.
    n = name.strip().lower()
    if n in ("advantages", "disadvantages", "core skills", "skills", "traits"):
        return True
    # Lines ending with ":" often indicate a grouping label
    if name.strip().endswith(":"):
        return True
    # If no points/ref and short-ish label, likely a group label
    if points is None and not ref and len(name) <= 40 and name[:1].isupper():
        return True
    return False


def build_bullet_tree(lines: List[str], default_context: str) -> Tuple[List[Node], List[Dict[str, Any]]]:
    """
    Build a raw indentation-based tree of bullet nodes.
    Context is determined by nearest heading above each bullet.
    """
    roots: List[Node] = []
    stack: List[Node] = []
    errors: List[Dict[str, Any]] = []

    current_context = default_context
    current_heading_path: List[str] = []

    def current_path_key(extra: str) -> str:
        base = " / ".join(current_heading_path) if current_heading_path else ""
        if base:
            return base + " / " + extra
        return extra

    for i, line in enumerate(lines, start=1):
        h = HEADING_RE.match(line)
        if h:
            lvl = len(h.group(1))
            title = normalize_title(h.group(2))
            # adjust heading path stack by level
            # simple approach: truncate to lvl-1 and append
            if lvl <= 1:
                current_heading_path = [title]
            else:
                current_heading_path = current_heading_path[: max(0, lvl - 1)]
                current_heading_path.append(title)
            current_context = detect_context_from_heading(title) or current_context
            continue

        b = BULLET_RE.match(line)
        if not b:
            continue

        indent_spaces = len(b.group(1).replace("\t", "    "))
        text = b.group(3).strip()

        explicit_kind, text2 = parse_tag(text)
        pts, text3 = extract_points(text2)
        ref, text4 = extract_reference(text3)
        name = normalize_title(text4)

        # placeholder kind (final classification after child linking)
        node = Node(
            raw=text,
            line_no=i,
            indent=indent_spaces,
            kind=explicit_kind or "unknown",
            name=name.rstrip(":"),
            points=pts,
            reference=ref,
            children=[],
            context_hint=current_context,
            path_key=current_path_key(name),
        )

        # attach based on indentation
        while stack and indent_spaces <= stack[-1].indent:
            stack.pop()

        if stack:
            stack[-1].children.append(node)
        else:
            roots.append(node)

        stack.append(node)

    return roots, errors


def classify_tree(nodes: List[Node], errors: List[Dict[str, Any]]) -> None:
    """
    Classify nodes into group/trait/skill using:
    - explicit tags
    - context_hint
    - whether it has children
    - heuristic group labels
    """
    def walk(n: Node, parent_kind: Optional[str]) -> None:
        has_children = len(n.children) > 0

        # If explicitly set, keep it unless it's inconsistent (e.g. SKILL with children: still OK -> treat as group? no)
        if n.kind in ("trait", "skill", "group"):
            # If explicit trait/skill but has children, treat it as group containing that type? We keep group explicitly only.
            # Keep explicit trait/skill even with children, but downstream can interpret "container leaf". We'll coerce to group.
            if has_children and n.kind in ("trait", "skill"):
                # Coerce to group, but preserve intended leaf type in an error note (so you notice)
                errors.append({
                    "type": "explicit_kind_has_children",
                    "line": n.line_no,
                    "raw": n.raw,
                    "message": f'Explicit {n.kind.upper()} node has children; coercing to GROUP.',
                })
                n.kind = "group"
        else:
            # infer group-ness first
            if looks_like_group(n.name, n.points, n.reference, has_children):
                n.kind = "group"
            else:
                # leaf: infer by context
                if n.context_hint == "skills":
                    n.kind = "skill"
                elif n.context_hint == "traits":
                    n.kind = "trait"
                else:
                    n.kind = "unknown"
                    errors.append({
                        "type": "untyped_node",
                        "line": n.line_no,
                        "raw": n.raw,
                        "message": "Could not classify node as trait/skill/group (no explicit tag and unknown section context).",
                    })

        for c in n.children:
            walk(c, n.kind)

    for n in nodes:
        walk(n, None)


def split_traits_skills(roots: List[Node], errors: List[Dict[str, Any]]) -> Tuple[List[Node], List[Node]]:
    """
    Decide whether a top-level subtree belongs under traits or skills.
    Rule:
    - If root.context_hint is skills => skills
    - If root.context_hint is traits => traits
    - Else, use majority of leaf kinds under it
    """
    def leaf_kind_counts(n: Node) -> Dict[str, int]:
        counts = {"trait": 0, "skill": 0, "unknown": 0}
        def rec(x: Node) -> None:
            if not x.children and x.kind in counts:
                counts[x.kind] += 1
            for ch in x.children:
                rec(ch)
        rec(n)
        return counts

    traits: List[Node] = []
    skills: List[Node] = []

    for r in roots:
        if r.context_hint == "skills":
            skills.append(r)
            continue
        if r.context_hint == "traits":
            traits.append(r)
            continue

        counts = leaf_kind_counts(r)
        if counts["skill"] > counts["trait"]:
            skills.append(r)
        elif counts["trait"] > counts["skill"]:
            traits.append(r)
        else:
            # ambiguous; default to traits, but error
            traits.append(r)
            errors.append({
                "type": "ambiguous_root",
                "line": r.line_no,
                "raw": r.raw,
                "message": "Ambiguous root subtree; defaulted to traits. Add explicit SKILL:/TRAIT:/GROUP: tags or headings.",
            })

    return traits, skills


def count_leaf_kinds(nodes: List[Node]) -> Dict[str, int]:
    counts = {"trait": 0, "skill": 0, "group": 0, "unknown": 0}
    def rec(n: Node) -> None:
        counts[n.kind] = counts.get(n.kind, 0) + 1
        for c in n.children:
            rec(c)
    for n in nodes:
        rec(n)
    return counts


def main() -> int:
    ap = argparse.ArgumentParser(description="Markdown -> GCTSPEC preprocessor")
    ap.add_argument("markdown", help="Input Markdown file")
    ap.add_argument("-o", "--out", help="Output file (default: stdout)")
    ap.add_argument("--yaml", action="store_true", help="Emit YAML (requires PyYAML). Default is JSON.")
    ap.add_argument("--template-name", default=None, help="Override template name (otherwise derived from H1 or filename).")
    ap.add_argument("--default-context", default="unknown", choices=["unknown", "traits", "skills"], help="Default context before any headings.")
    ap.add_argument("--fail-on-errors", action="store_true", help="Exit non-zero if errors were recorded.")
    args = ap.parse_args()

    md_path = args.markdown
    with open(md_path, "r", encoding="utf-8") as f:
        lines = f.read().splitlines()

    # derive template name: first H1, else filename stem
    template_name = args.template_name
    if not template_name:
        for line in lines:
            m = HEADING_RE.match(line)
            if m and len(m.group(1)) == 1:
                template_name = normalize_title(m.group(2))
                break
    if not template_name:
        template_name = os.path.splitext(os.path.basename(md_path))[0]

    roots, errors = build_bullet_tree(lines, args.default_context)
    classify_tree(roots, errors)
    traits, skills = split_traits_skills(roots, errors)

    # Additional hard guard: if we saw any skill leaves anywhere but skills list is empty => error
    all_counts = count_leaf_kinds(roots)
    if all_counts["skill"] > 0 and len(skills) == 0:
        errors.append({
            "type": "skills_lost",
            "message": "Skill leaves were detected but no top-level skills subtree was produced (likely misclassification).",
        })

    spec: Dict[str, Any] = {
        "gct_spec_version": 1,
        "template_name": template_name,
        "source_markdown": os.path.abspath(md_path),
        "generated_at": dt.datetime.now(dt.timezone.utc).isoformat(),
        "traits": [n.to_spec() for n in traits],
        "skills": [n.to_spec() for n in skills],
        "spells": [],
        "notes": [],
        "stats": {
            "nodes_total": len(roots),
            "leaf_kind_counts": all_counts,
            "roots_traits": len(traits),
            "roots_skills": len(skills),
            "errors": len(errors),
        },
        "errors": errors,
    }

    out_text: str
    if args.yaml:
        try:
            import yaml  # type: ignore
        except Exception:
            print("[md2gctspec] ERROR: --yaml requested but PyYAML not installed. Try: pip install pyyaml", file=sys.stderr)
            return 2
        out_text = yaml.safe_dump(spec, sort_keys=False, allow_unicode=True)
    else:
        out_text = json.dumps(spec, indent=2, ensure_ascii=False)

    if args.out:
        with open(args.out, "w", encoding="utf-8") as f:
            f.write(out_text)
            f.write("\n")
    else:
        print(out_text)

    if args.fail_on_errors and errors:
        print(f"[md2gctspec] FAIL: {len(errors)} error(s) recorded; refusing to proceed.", file=sys.stderr)
        return 3

    return 0


if __name__ == "__main__":
    raise SystemExit(main())

