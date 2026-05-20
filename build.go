package main

import (
	"bytes"
	"fmt"
	"html/template"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	texttemplate "text/template"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
)

const outDir = "build"
const siteURL = "https://viniciuscestari.dev"

type Post struct {
	Slug  string
	Title string
	Date  string
	Body  template.HTML
	TOC   template.HTML
}

type Page struct {
	Title string
	Body  template.HTML
}

const postTmpl = `<!doctype html>
<html lang="en">

<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{.Title}} - Vinicius Cestari</title>
  <link rel="stylesheet" href="/style.css">
  <link rel="stylesheet" href="/syntax.css">
</head>

<body>

  <header class="site">
    <a class="title" href="/">Vinicius Cestari</a>
    <nav>
      <a href="/about">about</a>
      <a href="https://github.com/ViniciusCestarii" target="_blank" rel="noopener">github</a>
    </nav>
  </header>

  <main>
    <article>
      <header>
        <h1>{{.Title}}</h1>
        <time datetime="{{.Date}}">{{.Date}}</time>
      </header>

{{.TOC}}
{{.Body}}
      <p><a href="/">← back</a></p>
    </article>
  </main>

</body>

</html>
`

const pageTmpl = `<!doctype html>
<html lang="en">

<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{.Title}} - Vinicius Cestari</title>
  <link rel="stylesheet" href="style.css">
</head>

<body>

  <header class="site">
    <a class="title" href="/">Vinicius Cestari</a>
    <nav>
      <a href="/about">about</a>
      <a href="https://github.com/ViniciusCestarii" target="_blank" rel="noopener">github</a>
    </nav>
  </header>

  <main>
{{.Body}}
  </main>

</body>

</html>
`

const indexTmpl = `<!doctype html>
<html lang="en">

<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Vinicius Cestari</title>
  <meta name="description" content="Notes on Bitcoin, systems programming, and things I'm studying.">
  <link rel="stylesheet" href="style.css">
  <link rel="alternate" type="application/rss+xml" title="Vinicius Cestari" href="/rss.xml">
</head>

<body>

  <header class="site">
    <a class="title" href="/">Vinicius Cestari</a>
    <nav>
      <a href="/about">about</a>
      <a href="https://github.com/ViniciusCestarii" target="_blank" rel="noopener">github</a>
    </nav>
  </header>

  <main>
    <p>Notes on what I'm studying. Mostly Bitcoin, systems, and protocols.</p>

    <h2>Posts</h2>
    <ul class="posts">
{{- range .}}
      <li>
        <a href="posts/{{.Slug}}">{{.Title}}</a>
        <time datetime="{{.Date}}">{{.Date}}</time>
      </li>
{{- end}}
    </ul>
  </main>

</body>

</html>
`

const rssTmpl = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Vinicius Cestari</title>
    <link>` + siteURL + `</link>
    <description>Notes on Bitcoin, systems programming, and things I'm studying.</description>
{{- range .}}
    <item>
      <title>{{.Title}}</title>
      <link>` + siteURL + `/posts/{{.Slug}}</link>
      <pubDate>{{.Date}}</pubDate>
      <guid>` + siteURL + `/posts/{{.Slug}}</guid>
    </item>
{{- end}}
  </channel>
</rss>
`

func main() {
	if err := os.RemoveAll(outDir); err != nil {
		log.Fatal(err)
	}
	mustMkdir(outDir)
	mustMkdir(filepath.Join(outDir, "posts"))

	md := goldmark.New(
		goldmark.WithExtensions(
			extension.GFM,
			highlighting.NewHighlighting(
				highlighting.WithFormatOptions(chromahtml.WithClasses(true)),
			),
		),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
		goldmark.WithRendererOptions(html.WithUnsafe()),
	)

	// Posts
	postFiles, err := filepath.Glob("content/posts/*.md")
	if err != nil {
		log.Fatal(err)
	}
	var posts []Post
	for _, f := range postFiles {
		p, err := loadPost(f, md)
		if err != nil {
			log.Fatalf("%s: %v", f, err)
		}
		posts = append(posts, p)
	}
	sort.Slice(posts, func(i, j int) bool { return posts[i].Date > posts[j].Date })

	postT := template.Must(template.New("post").Parse(postTmpl))
	for _, p := range posts {
		out := filepath.Join(outDir, "posts", p.Slug+".html")
		writeTemplate(out, postT, p)
	}

	// Pages (everything in content/*.md)
	pageFiles, err := filepath.Glob("content/*.md")
	if err != nil {
		log.Fatal(err)
	}
	pageT := template.Must(template.New("page").Parse(pageTmpl))
	for _, f := range pageFiles {
		page, slug, err := loadPage(f, md)
		if err != nil {
			log.Fatalf("%s: %v", f, err)
		}
		out := filepath.Join(outDir, slug+".html")
		writeTemplate(out, pageT, page)
	}

	// Index
	idxT := template.Must(template.New("index").Parse(indexTmpl))
	writeTemplate(filepath.Join(outDir, "index.html"), idxT, posts)

	// RSS
	rssT := texttemplate.Must(texttemplate.New("rss").Parse(rssTmpl))
	writeTextTemplate(filepath.Join(outDir, "rss.xml"), rssT, posts)

	// Static assets
	if err := copyTree("static", outDir); err != nil {
		log.Fatal(err)
	}
}

func loadPost(path string, md goldmark.Markdown) (Post, error) {
	meta, body, err := readMd(path, md)
	if err != nil {
		return Post{}, err
	}
	slug := strings.TrimSuffix(filepath.Base(path), ".md")
	var toc template.HTML
	if meta["toc"] == "true" {
		toc = buildTOC(string(body))
	}
	body = addHeadingAnchors(body)
	return Post{Slug: slug, Title: meta["title"], Date: meta["date"], Body: body, TOC: toc}, nil
}

func addHeadingAnchors(body template.HTML) template.HTML {
	return template.HTML(headingRe.ReplaceAllStringFunc(string(body), func(s string) string {
		m := headingRe.FindStringSubmatch(s)
		level, id, inner := m[1], m[2], m[3]
		return fmt.Sprintf(`<h%s id="%s">%s <a class="anchor" href="#%s" aria-label="Link to this section">#</a></h%s>`, level, id, inner, id, level)
	}))
}

var headingRe = regexp.MustCompile(`(?s)<h([2-6]) id="([^"]+)">(.*?)</h[2-6]>`)
var tagRe = regexp.MustCompile(`<[^>]+>`)

func buildTOC(html string) template.HTML {
	matches := headingRe.FindAllStringSubmatch(html, -1)
	if len(matches) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<details class="toc"><summary>Table of Contents</summary><ul>`)
	depth := 1
	prev := int(matches[0][1][0] - '0')
	for i, m := range matches {
		level := int(m[1][0] - '0')
		id := m[2]
		text := strings.TrimSpace(tagRe.ReplaceAllString(m[3], ""))
		if i > 0 {
			switch {
			case level > prev:
				for j := 0; j < level-prev; j++ {
					b.WriteString("<ul>")
					depth++
				}
			case level < prev:
				for j := 0; j < prev-level; j++ {
					b.WriteString("</li></ul>")
					depth--
				}
				b.WriteString("</li>")
			default:
				b.WriteString("</li>")
			}
		}
		fmt.Fprintf(&b, `<li><a href="#%s">%s</a>`, id, template.HTMLEscapeString(text))
		prev = level
	}
	for j := 0; j < depth; j++ {
		b.WriteString("</li></ul>")
	}
	b.WriteString("</details>")
	return template.HTML(b.String())
}

func loadPage(path string, md goldmark.Markdown) (Page, string, error) {
	meta, body, err := readMd(path, md)
	if err != nil {
		return Page{}, "", err
	}
	slug := strings.TrimSuffix(filepath.Base(path), ".md")
	return Page{Title: meta["title"], Body: body}, slug, nil
}

func readMd(path string, md goldmark.Markdown) (map[string]string, template.HTML, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	meta, body, err := splitFrontmatter(raw)
	if err != nil {
		return nil, "", err
	}
	var rendered bytes.Buffer
	if err := md.Convert(body, &rendered); err != nil {
		return nil, "", err
	}
	return meta, template.HTML(rendered.String()), nil
}

func splitFrontmatter(raw []byte) (map[string]string, []byte, error) {
	if !bytes.HasPrefix(raw, []byte("---\n")) {
		return nil, nil, fmt.Errorf("missing frontmatter")
	}
	rest := raw[4:]
	end := bytes.Index(rest, []byte("\n---\n"))
	if end < 0 {
		return nil, nil, fmt.Errorf("unterminated frontmatter")
	}
	head := rest[:end]
	body := rest[end+5:]

	meta := map[string]string{}
	for _, line := range strings.Split(string(head), "\n") {
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			return nil, nil, fmt.Errorf("bad frontmatter line: %q", line)
		}
		meta[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return meta, body, nil
}

func writeTextTemplate(path string, t *texttemplate.Template, data any) {
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		log.Fatal(err)
	}
	fmt.Println("wrote", path)
}

func writeTemplate(path string, t *template.Template, data any) {
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		log.Fatal(err)
	}
	fmt.Println("wrote", path)
}

func mustMkdir(p string) {
	if err := os.MkdirAll(p, 0755); err != nil {
		log.Fatal(err)
	}
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		defer out.Close()
		if _, err := io.Copy(out, in); err != nil {
			return err
		}
		fmt.Println("wrote", target)
		return nil
	})
}
