import type { ReactNode } from 'react';

/**
 * A left-to-right chain of steps that stacks on narrow screens. Use it to show
 * how a request travels, for example from kubectl to the keychain.
 */
export function Flow({ children, caption }: { children: ReactNode; caption?: ReactNode }) {
  return (
    <figure className="not-prose my-8">
      <ol className="flex flex-col md:flex-row md:items-stretch gap-0 md:gap-0 [counter-reset:flow]">
        {children}
      </ol>
      {caption ? (
        <figcaption className="mt-3 text-center text-sm text-fd-muted-foreground">{caption}</figcaption>
      ) : null}
    </figure>
  );
}

export function FlowStep({
  title,
  children,
  tone = 'default',
}: {
  title: ReactNode;
  children?: ReactNode;
  tone?: 'default' | 'tug' | 'harbour' | 'safe';
}) {
  const ring =
    tone === 'tug'
      ? 'border-(--color-tug)/50 bg-(--color-tug)/[0.07]'
      : tone === 'harbour'
        ? 'border-(--color-harbour)/50 bg-(--color-harbour)/[0.07]'
        : tone === 'safe'
          ? 'border-emerald-500/40 bg-emerald-500/[0.06]'
          : 'bg-fd-card';
  return (
    <li className="group flex min-w-0 flex-col md:flex-1 md:flex-row md:items-stretch [counter-increment:flow]">
      <div className={`relative min-w-0 flex-1 rounded-xl border p-4 ${ring}`}>
        <span className="absolute -top-2.5 left-3 rounded-full border bg-fd-background px-2 text-[11px] font-mono text-fd-muted-foreground before:content-[counter(flow)]" />
        <p className="font-display font-semibold text-[15px] leading-tight">{title}</p>
        {children ? <div className="mt-1.5 text-[13px] leading-relaxed text-fd-muted-foreground">{children}</div> : null}
      </div>
      <span
        aria-hidden="true"
        className="self-center text-fd-muted-foreground group-last:hidden px-2 py-1 md:py-0 rotate-90 md:rotate-0 select-none"
      >
        →
      </span>
    </li>
  );
}
