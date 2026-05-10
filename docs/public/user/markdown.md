# Markdown reference

shithub renders user-authored markdown — issue bodies, comments,
PR descriptions, READMEs — through a CommonMark + GFM parser
with a UGC-safe sanitizer. The set we support is close to
GitHub's, with a few deliberate omissions.

## Basics

```
# Heading 1
## Heading 2
### Heading 3

**bold**, *italic*, ~~strikethrough~~, `inline code`.

> A blockquote.

- bullet
- list

1. ordered
2. list

[link text](https://example.com)

![alt text](https://example.com/image.png)

---  (horizontal rule)
```

## Code

Three-backtick fenced blocks with an optional language tag. The
language drives syntax highlighting (chroma).

````
```go
func main() {
    fmt.Println("hello")
}
```
````

## Tables

GFM-style:

```
| Column A | Column B |
|----------|----------|
| cell 1   | cell 2   |
| cell 3   | cell 4   |
```

Alignment with `:`:

```
| Left | Center | Right |
|:-----|:------:|------:|
```

## Task lists

```
- [x] done
- [ ] todo
```

Checkboxes are clickable in issues and PRs you can edit.

## References

shithub auto-links these in any user-authored markdown:

- `#123` — issue or PR in the current repo.
- `owner/repo#123` — issue/PR in another repo.
- `@username` — user mention; notifies them.
- `@org/team` — team mention.
- A SHA (full or 7+ chars) — commit link.

## Emoji shortcodes

`:rocket:` → 🚀, `:eyes:` → 👀, etc. The set matches GitHub's
shortcode list.

## What we don't support

- **Inline HTML** — sanitized away. Use markdown alternatives.
- **`<style>` / `<script>`** — sanitized away.
- **Custom HTML attributes** (`onclick`, `style`, `id` on arbitrary
  elements) — stripped.
- **Footnotes** — not yet (planned).
- **MathJax / KaTeX** — not yet (planned).
- **Mermaid diagrams** — not yet (post-MVP).

README-style HTML alignment (`align="center"` on headings,
paragraphs, or divs) and image dimensions (`<img width="200">`) are
preserved because GitHub READMEs commonly use them for logos and
badges.

## Why a sanitizer?

User-authored markdown is the largest XSS surface on a forge.
shithub renders through a single helper (`internal/markdown`)
that runs every input through a `bluemonday` UGC policy after
parsing. Anything outside the supported set is silently dropped,
not preserved as escaped text.

## Previewing

Issue and PR forms have a "Preview" tab that renders the markdown
through the exact same pipeline we use for the saved version.
What you see in preview is what you'll see after submit.
