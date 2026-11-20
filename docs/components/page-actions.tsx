'use client';

import { BotMessageSquare, ChevronDown, FileText, GitBranch, MessageSquare } from 'lucide-react';
import { buttonVariants } from 'fumadocs-ui/components/ui/button';
import { Popover, PopoverContent, PopoverTrigger } from 'fumadocs-ui/components/ui/popover';
import type { ReactNode } from 'react';

/**
 * The page's "Open" menu: the source on GitHub, the Markdown version, and
 * two assistants that can read the page.
 */
export function PageActions({
  markdownUrl,
  githubUrl,
  pageUrl,
}: {
  markdownUrl: string;
  githubUrl: string;
  pageUrl: string;
}) {
  const prompt = encodeURIComponent(`Read ${pageUrl}, I want to ask questions about it.`);
  const items: { href: string; icon: ReactNode; label: string }[] = [
    { href: githubUrl, icon: <GitBranch />, label: 'Open on GitHub' },
    { href: markdownUrl, icon: <FileText />, label: 'View as Markdown' },
    { href: `https://claude.ai/new?q=${prompt}`, icon: <MessageSquare />, label: 'Ask Claude about this page' },
    { href: `https://chatgpt.com/?prompt=${prompt}&hints=search`, icon: <BotMessageSquare />, label: 'Ask ChatGPT about this page' },
  ];

  return (
    <Popover>
      <PopoverTrigger className={buttonVariants({ color: 'secondary', size: 'sm', className: 'gap-2' })}>
        Open
        <ChevronDown className="size-3.5 text-fd-muted-foreground" />
      </PopoverTrigger>
      <PopoverContent className="flex flex-col p-1" align="start">
        {items.map((item) => (
          <a
            key={item.label}
            href={item.href}
            rel="noreferrer noopener"
            target="_blank"
            className="flex items-center gap-2.5 rounded-lg p-2 text-sm hover:bg-fd-accent hover:text-fd-accent-foreground [&_svg]:size-4 [&_svg]:text-fd-muted-foreground"
          >
            {item.icon}
            {item.label}
          </a>
        ))}
      </PopoverContent>
    </Popover>
  );
}
