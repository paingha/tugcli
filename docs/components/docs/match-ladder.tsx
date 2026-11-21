import { rungs } from './match-ladder-data';

/** The order tug ranks matches in, best first, for the query "api". */
export function MatchLadder() {
  return (
    <figure className="not-prose my-8 rounded-xl border bg-fd-card overflow-hidden">
      <div className="flex items-center justify-between gap-4 border-b px-4 py-2.5 text-sm">
        <span className="text-fd-muted-foreground">
          You typed <code className="rounded bg-fd-muted px-1.5 py-0.5 font-mono text-fd-foreground">api</code>
        </span>
        <span className="text-xs uppercase tracking-wider text-fd-muted-foreground">best match first</span>
      </div>
      <ol>
        {rungs.map((r, i) => (
          <li
            key={r.rank}
            className="grid grid-cols-[2rem_1fr] sm:grid-cols-[2rem_9rem_11rem_1fr] items-center gap-x-3 gap-y-0.5 border-b last:border-b-0 px-4 py-2.5"
          >
            <span
              className="flex size-6 items-center justify-center rounded-full text-[11px] font-bold font-mono"
              style={{
                background: `color-mix(in oklab, var(--color-tug) ${Math.round(90 - i * 14)}%, transparent)`,
                color: i < 3 ? 'var(--color-fd-primary-foreground)' : 'var(--color-fd-foreground)',
              }}
            >
              {i + 1}
            </span>
            <span className="font-semibold text-sm">{r.rank}</span>
            <span className="col-start-2 sm:col-start-auto font-mono text-[13px]">
              <Highlight name={r.example} />
            </span>
            <span className="col-start-2 sm:col-start-auto text-[13px] text-fd-muted-foreground">{r.means}</span>
          </li>
        ))}
      </ol>
    </figure>
  );
}

/** Marks the letters of "api" that match, left to right. */
function Highlight({ name }: { name: string }) {
  const target = 'api';
  let t = 0;
  const lower = name.toLowerCase();
  const start = lower.indexOf(target);
  return (
    <>
      {name.split('').map((ch, i) => {
        let hit = false;
        if (start >= 0) {
          hit = i >= start && i < start + target.length;
        } else if (t < target.length && lower[i] === target[t]) {
          hit = true;
          t++;
        }
        return (
          <span key={i} className={hit ? 'text-(--color-tug) font-bold' : 'text-fd-muted-foreground'}>
            {ch}
          </span>
        );
      })}
    </>
  );
}
