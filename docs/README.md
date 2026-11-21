# tug docs

The user guide for tug, built with [Fumadocs](https://fumadocs.dev) on
Next.js and exported as a static site.

```bash
npm install
npm run dev        # http://localhost:3000
npm run build      # static site in out/
npm start          # serve out/ locally
```

Node 20.9 or newer runs the site; the Fumadocs scaffolder and file watcher
prefer Node 22.

## Where things are

| Path | What it holds |
| --- | --- |
| `content/docs/` | The pages, in MDX. Each folder's `meta.json` sets the sidebar order. |
| `app/(home)/page.tsx` | The landing page. |
| `components/home/` | Landing page pieces: the terminal demo and the night harbour scene. |
| `components/docs/` | MDX components for pages: `Flow`, `MatchLadder`, `Prompt`, `PromptModes`, `Icon`. |
| `app/global.css` | The Harbour theme: colours for light and dark, fonts and animations. |

Pages use British spelling, as the rest of tug's text does. Sample output in
the pages is copied from real runs of tug; when a command's output changes,
update the page that shows it.

## Building for a host

Three environment variables shape a build:

| Variable | Example | What it sets |
| --- | --- | --- |
| `DOCS_BASE_PATH` | `/tugcli` | The sub-path the site is served from. |
| `DOCS_SITE_URL` | `https://paingha.github.io/tugcli` | The public address, including the base path, for canonical URLs, the sitemap and link previews. |
| `DOCS_VERSION` | `v0.4.0` | The tug version shown in the sidebar. Default: `main`. |

```bash
DOCS_BASE_PATH=/tugcli DOCS_SITE_URL=https://paingha.github.io/tugcli npm run build
```

`.github/workflows/docs.yml` builds the site on every pull request that
touches `docs/`, and publishes it to GitHub Pages from `main`. Turn Pages on
first (Settings, Pages, source: GitHub Actions). Each page's "last updated"
date comes from git, so the workflow checks out the full history.

## What the build produces

Besides the pages: a branded `404.html`, `sitemap.xml`, `robots.txt`, a
link-preview image for every page under `og/`, and plain Markdown of every
page under `llms.mdx/`, collected in `llms.txt` and `llms-full.txt`. The
Markdown comes from `lib/markdown.ts`, which turns the MDX components into
text; teach it about any new component you add.

In ` ```console ` blocks, lines starting with `$ ` are commands and the rest
is output: output is dimmed, and the copy button copies only the commands.
Use ` ```text ` for output on its own.

`docs/go.mod` keeps this folder out of tug's Go module, so `go build ./...`
and golangci-lint never look inside `node_modules`.
