package main

import (
	"bytes"
	"fmt"
	"html/template"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
	"github.com/yuin/goldmark/renderer/html"
)

const outDir = "build"

type Post struct {
	Slug  string
	Title string
	Date  string
	Body  template.HTML
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
      <a href="/about.html">about</a>
      <a href="https://github.com/ViniciusCestarii" target="_blank" rel="noopener">github</a>
    </nav>
  </header>

  <main>
    <article>
      <header>
        <h1>{{.Title}}</h1>
        <time datetime="{{.Date}}">{{.Date}}</time>
      </header>

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
      <a href="/about.html">about</a>
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
</head>

<body>

  <header class="site">
    <a class="title" href="/">Vinicius Cestari</a>
    <nav>
      <a href="/about.html">about</a>
      <a href="https://github.com/ViniciusCestarii" target="_blank" rel="noopener">github</a>
    </nav>
  </header>

  <main>
    <p>Notes on what I'm studying. Mostly Bitcoin, systems, and protocols.</p>

    <h2>Posts</h2>
    <ul class="posts">
{{- range .}}
      <li>
        <a href="posts/{{.Slug}}.html">{{.Title}}</a>
        <time datetime="{{.Date}}">{{.Date}}</time>
      </li>
{{- end}}
    </ul>
  </main>

</body>

</html>
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
	return Post{Slug: slug, Title: meta["title"], Date: meta["date"], Body: body}, nil
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
