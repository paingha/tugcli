// Turns a page's MDX source into plain Markdown for "Copy Markdown",
// /llms.txt and /llms-full.txt. The docs' components become text a person or
// a language model can read: steps become plain sections, tabs become
// labelled blocks, cards become links, and the figures drawn from code (the
// match ladder, the prompt colours, type tables) become lists and tables.
//
// The pages are this site's own source, read at build time.

import { rungs } from '@/components/docs/match-ladder-data';

type Attrs = Record<string, string>;

function attrs(source: string): Attrs {
  const out: Attrs = {};
  for (const m of source.matchAll(/(\w+)="([^"]*)"/g)) out[m[1]] = m[2];
  return out;
}

/** Removes the indentation common to every non-blank line, and outer blank lines. */
function dedent(text: string): string {
  const lines = text.replace(/^\s*\n/, '').replace(/\n\s*$/, '').split('\n');
  const indents = lines.filter((l) => l.trim()).map((l) => l.match(/^ */)![0].length);
  const cut = indents.length ? Math.min(...indents) : 0;
  return lines.map((l) => l.slice(cut)).join('\n');
}

const oneLine = (text: string) => dedent(text).replace(/\s*\n\s*/g, ' ').trim();
const cell = (text: unknown) => String(text ?? '').replace(/\|/g, '\\|').replace(/\n/g, ' ');

interface TypeNode {
  type?: string;
  description?: string;
  default?: string;
  required?: boolean;
}

function typeTable(body: string): string {
  let fields: Record<string, TypeNode>;
  try {
    fields = new Function(`return ({${body}});`)() as Record<string, TypeNode>;
  } catch {
    return '';
  }
  const rows = Object.entries(fields).map(
    ([name, f]) =>
      `| \`${cell(name)}\`${f.required ? ' (required)' : ''} | ${cell(f.type)} | ${cell(f.default)} | ${cell(f.description)} |`,
  );
  return ['| Field | Type | Default | Description |', '| --- | --- | --- | --- |', ...rows].join('\n');
}

function matchLadder(): string {
  return [
    'How tug ranks matches for the query `api`, best first:',
    '',
    '| Rank | Example | Means |',
    '| --- | --- | --- |',
    ...rungs.map((r, i) => `| ${i + 1}. ${r.rank} | \`${r.example}\` | ${r.means} |`),
  ].join('\n');
}

function promptLine(a: Attrs): string {
  const where = `⎈ ${a.cluster ?? 'staging-us'}:${a.ns ?? 'default'}`;
  const suffix = a.mode === 'readonly' ? ' (read-only)' : a.mode === 'unlocked' ? ' (unlocked)' : '';
  const command = a.command ? ` ${a.command}` : '';
  return `\`${where}${suffix} ~/src $${command}\``;
}

function promptModes(a: Attrs): string {
  const cluster = a.cluster ?? 'prod-eu';
  const ns = a.ns ?? 'payments';
  return [
    `- **Read-write**: \`⎈ staging-us:${ns}\` in cyan. Changes are allowed.`,
    `- **Read-only**: \`⎈ ${cluster}:${ns} (read-only)\` in bold red. tug refuses or asks before changes.`,
    `- **Unlocked**: \`⎈ ${cluster}:${ns} (unlocked)\` in bold yellow. A read-only cluster, open for a while.`,
  ].join('\n');
}

function files(body: string): string {
  const out: string[] = [];
  let depth = 0;
  for (const raw of body.split('\n')) {
    const line = raw.trim();
    const folder = line.match(/^<Folder\b([^>]*?)(\/)?>/);
    const file = line.match(/^<File\b([^>]*?)\/>/);
    if (folder) {
      out.push(`${'  '.repeat(depth)}- ${attrs(folder[1]).name}/`);
      if (!folder[2]) depth++;
    } else if (file) {
      out.push(`${'  '.repeat(depth)}- ${attrs(file[1]).name}`);
    } else if (line.startsWith('</Folder>')) {
      depth = Math.max(0, depth - 1);
    }
  }
  return out.join('\n');
}

export function toMarkdown(mdx: string): string {
  let s = mdx.replace(/^---\n[\s\S]*?\n---\n/, '');

  s = s.replace(/<TypeTable\s+type=\{\{([\s\S]*?)\}\}\s*\/>/g, (_, body: string) => typeTable(body));
  s = s.replace(/<MatchLadder\s*\/>/g, matchLadder);
  s = s.replace(/<PromptModes\b([^>]*?)\/>/g, (_, a: string) => promptModes(attrs(a)));
  s = s.replace(/<Prompt\b([^>]*?)\/>/g, (_, a: string) => promptLine(attrs(a)));
  s = s.replace(/<Icon\b[^>]*?\/>/g, '');

  // Containers first, outermost to innermost, so each body is dedented
  // before the components inside it are converted.
  s = s.replace(/[ \t]*<Tab\s+value="([^"]*)"\s*>([\s\S]*?)<\/Tab>/g, (_, v: string, body: string) => `\n**${v}**\n\n${dedent(body)}`);
  s = s.replace(/[ \t]*(?:<Tabs\b[^>]*>|<\/Tabs>)/g, '');
  s = s.replace(/[ \t]*<\/?Steps?>/g, '');

  s = s.replace(/[ \t]*<Accordion\b([^>]*)>([\s\S]*?)<\/Accordion>/g, (_, a: string, body: string) => {
    return `\n**${attrs(a).title}**\n\n${dedent(body)}`;
  });
  s = s.replace(/[ \t]*<\/?Accordions>/g, '');

  s = s.replace(/[ \t]*<Callout\b([^>]*)>([\s\S]*?)<\/Callout>/g, (_, a: string, body: string) => {
    const { title, type } = attrs(a);
    const label = title ?? (type === 'warn' || type === 'warning' ? 'Warning' : 'Note');
    const quoted = dedent(body)
      .split('\n')
      .map((l) => (l ? `> ${l}` : '>'));
    return [`> **${label}**`, '>', ...quoted].join('\n');
  });

  s = s.replace(/<Flow\b([^>]*)>([\s\S]*?)<\/Flow>/g, (_, a: string, body: string) => {
    const steps = [...body.matchAll(/<FlowStep\b([^>]*)>([\s\S]*?)<\/FlowStep>/g)].map(
      (m) => `1. **${attrs(m[1]).title}**: ${oneLine(m[2])}`,
    );
    const caption = attrs(a).caption;
    return steps.join('\n') + (caption ? `\n\n*${caption}*` : '');
  });

  s = s.replace(/[ \t]*<Card\b([^>]*?)(?:\/>|>([\s\S]*?)<\/Card>)/g, (_, a: string, body?: string) => {
    const { title, href } = attrs(a);
    const label = href ? `[${title}](${href})` : `**${title}**`;
    return `- ${label}${body ? `: ${oneLine(body)}` : ''}`;
  });
  s = s.replace(/[ \t]*<\/?Cards>/g, '');

  s = s.replace(/<Files>([\s\S]*?)<\/Files>/g, (_, body: string) => files(body));

  // Lines left with only spaces where a tag was removed.
  s = s.replace(/^[ \t]+$/gm, '');

  return s.replace(/\n{3,}/g, '\n\n').trim() + '\n';
}
