// Shiki transformer for `console` code blocks: terminal transcripts where
// lines starting with "$ " are commands and everything else is output.
//
// Output lines get the class `console-output` (dimmed in global.css), and
// both output and the "$ " prompt get `nd-copy-ignore`, which Fumadocs' copy
// button leaves out. Copying a transcript then gives just the commands.

import type { Element, ElementContent, Text } from 'hast';
import type { ShikiTransformer } from 'shiki';

const PROMPT = '$ ';

function classes(node: Element): string[] {
  const c = node.properties.className ?? node.properties.class ?? [];
  return Array.isArray(c) ? c.map(String) : String(c).split(/\s+/).filter(Boolean);
}

function isLine(node: ElementContent): node is Element {
  return node.type === 'element' && classes(node).includes('line');
}

function textOf(node: ElementContent): string {
  if (node.type === 'text') return node.value;
  if (node.type === 'element') return node.children.map(textOf).join('');
  return '';
}

function addClass(node: Element, ...names: string[]) {
  node.properties.className = [...classes(node), ...names];
  delete node.properties.class;
}

/** Removes the first n characters of text from a line's token spans. */
function dropLeading(node: Element, n: number): number {
  for (const child of node.children) {
    if (n === 0) break;
    if (child.type === 'text') {
      const take = Math.min(n, child.value.length);
      child.value = child.value.slice(take);
      n -= take;
    } else if (child.type === 'element') {
      n = dropLeading(child, n);
    }
  }
  return n;
}

export function transformerConsole(): ShikiTransformer {
  return {
    name: 'tug:console',
    code(code) {
      const lang = this.options.lang;
      if (lang !== 'console' && lang !== 'shellsession') return;

      // Move each newline into the line before it, so leaving a line out of
      // the copied text leaves its newline out too.
      const kids: ElementContent[] = [];
      for (const node of code.children) {
        const prev = kids[kids.length - 1];
        if (node.type === 'text' && node.value === '\n' && prev && isLine(prev)) {
          prev.children.push(node as Text);
          continue;
        }
        kids.push(node);
      }
      code.children = kids;

      for (const line of kids) {
        if (!isLine(line)) continue;
        const text = textOf(line).replace(/\n$/, '');
        if (text.startsWith(PROMPT)) {
          dropLeading(line, PROMPT.length);
          line.children.unshift({
            type: 'element',
            tagName: 'span',
            properties: { className: ['console-prompt', 'nd-copy-ignore'] },
            children: [{ type: 'text', value: PROMPT }],
          });
        } else if (text.trim() !== '') {
          addClass(line, 'console-output', 'nd-copy-ignore');
        } else {
          addClass(line, 'nd-copy-ignore');
        }
      }
    },
  };
}
