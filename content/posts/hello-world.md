---
title: Hello, world
date: 2026-05-04
---

I had a blog in TypeScript before this one. React, MDX, Next.js. It was fun to set up, and I barely wrote in it. So I am trying the other direction: markdown, plain CSS, no JavaScript. One small Go file turns the markdown into the site, and `git push` is the deploy.

The thing I keep coming back to, in Bitcoin and in life, is that the tech is not really the point. What works is the point. A small site I actually update is worth more than a "clever" one I never touch, so I would rather lean on simplicity than fight it.

## How a post gets written

1. Create `content/posts/your-slug.md` with a `title` and `date`.
2. Write markdown. Drop in raw HTML when markdown can't say it.
3. `go run build.go`.

That is the whole pipeline. Good enough is the goal.

## Code looks like this

```cpp
#include <stdio.h>

int main(void) {
    printf("hello, world\n");
    return 0;
}
```

<div class="with-aside">

And inline `code` looks like that.

<aside class="side">
  <strong>Aside</strong>
  <p>This is an aside at the side</p>
</aside>

</div>

A table, for the sake of having one:

| Tool   | What for | Why                          |
| ------ | -------- | ---------------------------- |
| Arch   | OS, btw  | Build it the way I want it   |
| VSCode | Editor   | Just works, stays out of way |
| fish   | Shell    | Sane defaults, nice prompt   |

> If you get everything you want the minute you want it, what's the point of living? <br>
> *Someone smart*
