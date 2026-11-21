import type { ReactNode } from 'react';

import { cn } from '@/lib/cn';

type Mode = 'normal' | 'readonly' | 'unlocked';

// Prompts always sit on a dark terminal, so these are terminal colours.
const styles: Record<Mode, { colour: string; bold: boolean; label: string; note: string }> = {
  normal: {
    colour: '#5fd4dc',
    bold: false,
    label: 'Read-write',
    note: 'Cyan, with the env label. Changes are allowed.',
  },
  readonly: {
    colour: '#ff7b86',
    bold: true,
    label: 'Read-only',
    note: 'Bold red. tug refuses or asks before changes.',
  },
  unlocked: {
    colour: '#ffcf5c',
    bold: true,
    label: 'Unlocked',
    note: 'Bold yellow, with the time left. Read-only again when it runs out.',
  },
};

/** The flags in brackets after the cluster, as the hook writes them. */
function flags(mode: Mode, left: string, proxy: boolean): string {
  const out: string[] = [];
  if (mode === 'readonly') out.push('read-only');
  if (mode === 'unlocked') out.push(left ? `unlocked ${left}` : 'unlocked');
  if (proxy) out.push('proxy');
  return out.length ? ` (${out.join(', ')})` : '';
}

/** One shell prompt line as tug's hook draws it. */
export function Prompt({
  mode = 'normal',
  cluster = 'staging-us',
  ns = 'default',
  env = '',
  prod = false,
  left = '',
  proxy = false,
  command = '',
  className,
}: {
  mode?: Mode;
  cluster?: string;
  ns?: string;
  /** The cluster's env label, shown in brackets unless the badge shows it. */
  env?: string;
  /** A production cluster, which gets a red badge whatever the mode. */
  prod?: boolean;
  /** Time left on an unlock, such as 12m. */
  left?: string;
  /** The session goes through tugd. */
  proxy?: boolean;
  command?: string;
  className?: string;
}) {
  const s = styles[mode];
  const label = !prod && env ? ` [${env}]` : '';
  return (
    <div
      className={cn(
        'not-prose font-mono text-[13px] leading-6 rounded-lg border border-white/10 bg-[#0b1724] text-[#dbe6ef] px-4 py-2.5 overflow-x-auto whitespace-pre',
        className,
      )}
    >
      {prod && (
        <>
          <span style={{ background: '#c8303f', color: '#ffffff', fontWeight: 700 }}> {(env || 'prod').toUpperCase()} </span>{' '}
        </>
      )}
      <span style={{ color: s.colour, fontWeight: s.bold ? 700 : 400 }}>
        ⎈ {cluster}:{ns}
        {label}
        {flags(mode, left, proxy)}
      </span>{' '}
      <span className="text-[#8ea3b5]">~/src $</span> {command}
    </div>
  );
}

/** The prompt styles, with what each one means. */
export function PromptModes({ cluster = 'prod-eu', ns = 'payments' }: { cluster?: string; ns?: string }) {
  const rows: { key: string; label: string; note: string; prompt: ReactNode }[] = [
    { key: 'normal', ...styles.normal, prompt: <Prompt cluster="staging-us" ns={ns} env="staging" /> },
    { key: 'readonly', ...styles.readonly, prompt: <Prompt mode="readonly" cluster={cluster} ns={ns} env="prod" prod /> },
    { key: 'unlocked', ...styles.unlocked, prompt: <Prompt mode="unlocked" cluster={cluster} ns={ns} env="prod" prod left="12m" /> },
    {
      key: 'prod-rw',
      label: 'Production, read-write',
      note: 'The red badge marks production in every mode, even opened with --read-write.',
      prompt: <Prompt cluster={cluster} ns={ns} env="prod" prod />,
    },
  ];
  return (
    <div className="not-prose grid gap-4 my-6">
      {rows.map((r) => (
        <div key={r.key} className="grid gap-2 sm:grid-cols-[9rem_1fr] sm:items-center">
          <div>
            <p className="text-sm font-semibold">{r.label}</p>
            <p className="text-xs text-fd-muted-foreground">{r.note}</p>
          </div>
          {r.prompt}
        </div>
      ))}
    </div>
  );
}
