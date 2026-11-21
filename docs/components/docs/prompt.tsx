import { cn } from '@/lib/cn';

type Mode = 'normal' | 'readonly' | 'unlocked';

// Prompts always sit on a dark terminal, so these are terminal colours.
const styles: Record<Mode, { colour: string; bold: boolean; suffix: string; label: string; note: string }> = {
  normal: {
    colour: '#5fd4dc',
    bold: false,
    suffix: '',
    label: 'Read-write',
    note: 'Cyan. Changes are allowed.',
  },
  readonly: {
    colour: '#ff7b86',
    bold: true,
    suffix: ' (read-only)',
    label: 'Read-only',
    note: 'Bold red. tug refuses or asks before changes.',
  },
  unlocked: {
    colour: '#ffcf5c',
    bold: true,
    suffix: ' (unlocked)',
    label: 'Unlocked',
    note: 'Bold yellow. A read-only cluster, open for a while.',
  },
};

/** One shell prompt line as tug's hook draws it. */
export function Prompt({
  mode = 'normal',
  cluster = 'staging-us',
  ns = 'default',
  command = '',
  className,
}: {
  mode?: Mode;
  cluster?: string;
  ns?: string;
  command?: string;
  className?: string;
}) {
  const s = styles[mode];
  return (
    <div
      className={cn(
        'not-prose font-mono text-[13px] leading-6 rounded-lg border border-white/10 bg-[#0b1724] text-[#dbe6ef] px-4 py-2.5 overflow-x-auto whitespace-pre',
        className,
      )}
    >
      <span style={{ color: s.colour, fontWeight: s.bold ? 700 : 400 }}>
        ⎈ {cluster}:{ns}
        {s.suffix}
      </span>{' '}
      <span className="text-[#8ea3b5]">~/src $</span> {command}
    </div>
  );
}

/** The three prompt styles, with what each one means. */
export function PromptModes({ cluster = 'prod-eu', ns = 'payments' }: { cluster?: string; ns?: string }) {
  return (
    <div className="not-prose grid gap-4 my-6">
      {(Object.keys(styles) as Mode[]).map((mode) => (
        <div key={mode} className="grid gap-2 sm:grid-cols-[9rem_1fr] sm:items-center">
          <div>
            <p className="text-sm font-semibold">{styles[mode].label}</p>
            <p className="text-xs text-fd-muted-foreground">{styles[mode].note}</p>
          </div>
          <Prompt mode={mode} cluster={mode === 'normal' ? 'staging-us' : cluster} ns={ns} />
        </div>
      ))}
    </div>
  );
}
