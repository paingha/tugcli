'use client';

import { Check, Copy } from 'lucide-react';
import { useState } from 'react';
import { cn } from '@/lib/cn';

/** A one-line shell command with a copy button. */
export function CopyCommand({
  command,
  variant = 'default',
  className,
}: {
  command: string;
  variant?: 'default' | 'night';
  className?: string;
}) {
  const [copied, setCopied] = useState(false);
  const night = variant === 'night';
  return (
    <div
      className={cn(
        'flex w-full min-w-0 items-center gap-3 rounded-xl border px-4 py-3 font-mono text-[12.5px] sm:text-[13px]',
        night ? 'border-white/12 bg-white/[0.06] text-[#e6eef5]' : 'bg-fd-card',
        className,
      )}
    >
      <span className={cn('select-none', night ? 'text-[#ff7a47]' : 'text-(--color-tug)')}>$</span>
      <code className="min-w-0 flex-1 overflow-x-auto whitespace-nowrap [scrollbar-width:none]">{command}</code>
      <button
        type="button"
        aria-label={copied ? 'Copied' : 'Copy command'}
        onClick={() => {
          void navigator.clipboard.writeText(command).then(() => {
            setCopied(true);
            window.setTimeout(() => setCopied(false), 1600);
          });
        }}
        className={cn(
          'shrink-0 rounded-md p-1.5 transition-colors',
          night
            ? 'text-[#8ea3b5] hover:bg-white/10 hover:text-white'
            : 'text-fd-muted-foreground hover:bg-fd-accent hover:text-fd-accent-foreground',
        )}
      >
        {copied ? <Check className="size-4" /> : <Copy className="size-4" />}
      </button>
    </div>
  );
}
