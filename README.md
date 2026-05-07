# blog

Source for my blog. Markdown in, static HTML out.

## Layout

```
content/
  about.md            # pages
  posts/*.md          # blog posts (frontmatter: title, date)
static/               # files copied verbatim into build/ (style.css, etc.)
build/                # generated output, served as the site
build.go              # the build code
```

## Requirements

- Go 1.21+

## Build

```
go run build.go
```

Output goes to `build/`. The directory is wiped on each run.

## Serve locally

```
python -m http.server -d build 3000
```

Then open <http://localhost:3000>.

## Write a post

1. Create `content/posts/your-slug.md`:

   ```
   ---
   title: Your title
   date: 2026-05-06
   ---

   Markdown body here. Raw HTML works when markdown isn't enough.
   ```

2. `go run build.go`.
3. `git push`.
