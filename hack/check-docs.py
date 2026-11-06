#!/usr/bin/env python3
"""Checks that the relative links of the Markdown files resolve.

    python3 hack/check-docs.py        (make test-docs)

Every [text](target) and [ref]: target whose target is not a URL must point at
an existing file or directory; if it has a #fragment and the target is a
Markdown file, a heading with that GitHub-style anchor must exist. Links inside
code blocks and code spans are ignored, URLs are not fetched.
"""
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SKIP_DIRS = {".git", "node_modules", ".cache", ".out"}

INLINE_LINK = re.compile(r"(?<!\!)\[(?:[^\]\\]|\\.)*\]\(\s*<?([^)\s>]+)>?(?:\s+\"[^\"]*\")?\s*\)")
IMAGE_LINK = re.compile(r"!\[(?:[^\]\\]|\\.)*\]\(\s*<?([^)\s>]+)>?")
REF_DEF = re.compile(r"^\s{0,3}\[[^\]]+\]:\s*<?(\S+?)>?(?:\s+.*)?$")
HEADING = re.compile(r"^\s{0,3}(#{1,6})\s+(.*?)\s*#*\s*$")
URL = re.compile(r"^[a-zA-Z][a-zA-Z0-9+.-]*:")


def markdown_files():
    for dirpath, dirnames, filenames in os.walk(ROOT):
        dirnames[:] = [d for d in dirnames if d not in SKIP_DIRS]
        for name in filenames:
            if name.endswith(".md"):
                yield os.path.join(dirpath, name)


def strip_code(lines, spans=True):
    """Yields (line number, text) outside fenced code blocks, without code spans."""
    fence = None
    for n, line in enumerate(lines, 1):
        m = re.match(r"^\s{0,3}(`{3,}|~{3,})", line)
        if m:
            if fence is None:
                fence = m.group(1)[0] * len(m.group(1))
            elif line.strip().startswith(fence):
                fence = None
            continue
        if fence is None:
            yield n, re.sub(r"`+[^`]*`+", "", line) if spans else line


def slug(heading):
    text = re.sub(r"\[([^\]]*)\]\([^)]*\)", r"\1", heading)  # links keep their text
    text = re.sub(r"<[^>]+>", "", text)
    text = text.replace("`", "").replace("*", "").strip().lower()
    out = []
    for ch in text:
        if ch.isalnum() or ch in "-_":
            out.append(ch)
        elif ch == " ":
            out.append("-")
    return "".join(out)


_anchors = {}


def anchors(path):
    if path not in _anchors:
        seen = {}
        result = set()
        with open(path, encoding="utf-8") as f:
            for _, line in strip_code(f.read().splitlines(), spans=False):
                m = HEADING.match(line)
                if not m:
                    continue
                s = slug(m.group(2))
                count = seen.get(s, 0)
                seen[s] = count + 1
                result.add(s if count == 0 else f"{s}-{count}")
        _anchors[path] = result
    return _anchors[path]


def main():
    errors = []
    checked = 0
    for path in sorted(markdown_files()):
        rel = os.path.relpath(path, ROOT)
        with open(path, encoding="utf-8") as f:
            lines = f.read().splitlines()
        for n, text in strip_code(lines):
            targets = INLINE_LINK.findall(text) + IMAGE_LINK.findall(text)
            m = REF_DEF.match(text)
            if m:
                targets.append(m.group(1))
            for target in targets:
                if URL.match(target) or target.startswith("//"):
                    continue
                checked += 1
                file_part, _, fragment = target.partition("#")
                if file_part:
                    dest = os.path.normpath(os.path.join(os.path.dirname(path), file_part))
                else:
                    dest = path
                if not os.path.exists(dest):
                    errors.append(f"{rel}:{n}: {target}: no such file")
                    continue
                if fragment and dest.endswith(".md") and os.path.isfile(dest):
                    if fragment.lower() not in anchors(dest):
                        errors.append(f"{rel}:{n}: {target}: no such heading in {os.path.relpath(dest, ROOT)}")
    for e in errors:
        print(e, file=sys.stderr)
    if errors:
        print(f"{len(errors)} broken link(s)", file=sys.stderr)
        return 1
    print(f"{checked} relative links in {len(list(markdown_files()))} Markdown files resolve")
    return 0


if __name__ == "__main__":
    sys.exit(main())
