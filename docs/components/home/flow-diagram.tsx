// Where clusters come from and where they go: sources feed tug, and tug
// hands every tool the same session. On wide screens the sources sit on the
// left and the tools on the right; on phones the picture runs top to bottom.

import type { ReactNode } from 'react';
import { Boxes, Cloud, FileCode, Laptop, Puzzle, ShipWheel, SquareTerminal, Telescope, Zap } from 'lucide-react';
import { LogoMark } from '@/components/logo';

type Item = { icon: ReactNode; name: string; detail: string; tint: string };

const sources: Item[] = [
  {
    icon: <FileCode />,
    name: 'kubeconfig files',
    detail: '~/.kube/config and friends',
    tint: 'bg-[#e8efff] text-[#3166d6] dark:bg-[#3166d6]/20 dark:text-[#9dbbff]',
  },
  {
    icon: <Laptop />,
    name: 'Local clusters',
    detail: 'kind, k3d, minikube, OrbStack',
    tint: 'bg-[#e3f6ec] text-[#1f8a55] dark:bg-[#1f8a55]/20 dark:text-[#7fe0ad]',
  },
  {
    icon: <Cloud />,
    name: 'Cloud accounts',
    detail: 'EKS, GKE, AKS, DigitalOcean…',
    tint: 'bg-[#fff0e6] text-[#d2601f] dark:bg-[#d2601f]/20 dark:text-[#ffb27d]',
  },
  {
    icon: <Boxes />,
    name: 'Platforms',
    detail: 'Cluster API, Argo CD, Rancher',
    tint: 'bg-[#f1ebff] text-[#7347d1] dark:bg-[#7347d1]/25 dark:text-[#c3a9ff]',
  },
  {
    icon: <Puzzle />,
    name: 'Your own plugin',
    detail: 'any language that speaks JSON',
    tint: 'bg-[#e4f6f7] text-[#16808a] dark:bg-[#16808a]/25 dark:text-[#7fdbe2]',
  },
];

const tools: Item[] = [
  {
    icon: <SquareTerminal />,
    name: 'kubectl',
    detail: 'runs in your session',
    tint: 'bg-[#eef0f3] text-[#33404f] dark:bg-white/10 dark:text-[#dbe6ef]',
  },
  {
    icon: <ShipWheel />,
    name: 'helm',
    detail: 'same cluster, no flags',
    tint: 'bg-[#eef0f3] text-[#33404f] dark:bg-white/10 dark:text-[#dbe6ef]',
  },
  {
    icon: <Telescope />,
    name: 'k9s',
    detail: 'opened with tug ui',
    tint: 'bg-[#eef0f3] text-[#33404f] dark:bg-white/10 dark:text-[#dbe6ef]',
  },
  {
    icon: <Zap />,
    name: 'tug logs, sh, pf',
    detail: 'short names, every pod',
    tint: 'bg-[#eef0f3] text-[#33404f] dark:bg-white/10 dark:text-[#dbe6ef]',
  },
];

// Cards are a fixed height so the connectors can find their centres.
const CARD = 60;
const GAP = 12;

function centres(n: number, rows: number): number[] {
  const height = rows * CARD + (rows - 1) * GAP;
  const offset = (height - (n * CARD + (n - 1) * GAP)) / 2;
  return Array.from({ length: n }, (_, i) => ((offset + i * (CARD + GAP) + CARD / 2) / height) * 100);
}

function ItemCard({ item, compact }: { item: Item; compact?: boolean }) {
  return (
    <li
      className="flex min-w-0 items-center gap-3 rounded-2xl border bg-fd-background px-3 shadow-[0_1px_2px_rgba(15,23,42,0.04),0_8px_24px_-16px_rgba(15,23,42,0.25)]"
      style={compact ? { paddingBlock: 10 } : { height: CARD }}
    >
      <span className={`flex size-9 shrink-0 items-center justify-center rounded-xl [&_svg]:size-[18px] ${item.tint}`}>
        {item.icon}
      </span>
      <span className="min-w-0">
        <span className="block truncate text-[14px] font-semibold leading-tight">{item.name}</span>
        <span className="block truncate text-[12.5px] leading-snug text-fd-muted-foreground">{item.detail}</span>
      </span>
    </li>
  );
}

function Hub() {
  return (
    <div className="relative mx-auto flex size-24 items-center justify-center">
      <span
        aria-hidden="true"
        className="absolute inset-0 rounded-[28px] bg-(--color-tug)/25 motion-safe:animate-[ping-soft_2.6s_ease-out_infinite]"
      />
      <span className="relative flex size-24 items-center justify-center rounded-[28px] bg-fd-foreground text-fd-background shadow-[0_20px_40px_-18px_rgba(15,23,42,0.6)]">
        <LogoMark className="size-14" />
      </span>
    </div>
  );
}

/** Curves from each card's centre on one side to the hub's on the other. */
function Connectors({ ys, from }: { ys: number[]; from: 'left' | 'right' }) {
  return (
    <svg viewBox="0 0 100 100" preserveAspectRatio="none" aria-hidden="true" className="h-full w-full overflow-visible">
      {ys.map((y) => {
        const d = from === 'left' ? `M0 ${y} C 55 ${y}, 45 50, 100 50` : `M0 50 C 55 50, 45 ${y}, 100 ${y}`;
        return (
          <g key={y}>
            <path d={d} fill="none" stroke="var(--color-fd-border)" strokeWidth="1.5" vectorEffect="non-scaling-stroke" />
            <path
              d={d}
              fill="none"
              stroke="var(--color-tug)"
              strokeWidth="1.5"
              strokeLinecap="round"
              vectorEffect="non-scaling-stroke"
              className="flow-line"
              opacity="0.8"
            />
          </g>
        );
      })}
    </svg>
  );
}

function Drop() {
  return (
    <svg viewBox="0 0 2 40" aria-hidden="true" className="mx-auto my-2 h-10 w-0.5 overflow-visible">
      <path d="M1 0 V40" stroke="var(--color-fd-border)" strokeWidth="1.5" />
      <path d="M1 0 V40" stroke="var(--color-tug)" strokeWidth="1.5" strokeLinecap="round" className="flow-line" />
    </svg>
  );
}

export function FlowDiagram() {
  const rows = Math.max(sources.length, tools.length);
  return (
    <div aria-label="Sources such as kubeconfig files, local clusters and cloud accounts feed tug, which gives kubectl, helm, k9s and its own commands the same session">
      {/* Wide screens: left to right. */}
      <div className="hidden grid-cols-[minmax(0,1fr)_minmax(3rem,7rem)_auto_minmax(3rem,7rem)_minmax(0,1fr)] items-stretch md:grid">
        <ul className="flex flex-col justify-center" style={{ gap: GAP }}>
          {sources.map((s) => (
            <ItemCard key={s.name} item={s} />
          ))}
        </ul>
        <Connectors ys={centres(sources.length, rows)} from="left" />
        <div className="flex items-center px-2">
          <Hub />
        </div>
        <Connectors ys={centres(tools.length, rows)} from="right" />
        <ul className="flex flex-col justify-center" style={{ gap: GAP }}>
          {tools.map((t) => (
            <ItemCard key={t.name} item={t} />
          ))}
        </ul>
      </div>

      {/* Phones: top to bottom. */}
      <div className="md:hidden">
        <ul className="grid grid-cols-1 gap-2 min-[420px]:grid-cols-2">
          {sources.map((s) => (
            <ItemCard key={s.name} item={s} compact />
          ))}
        </ul>
        <Drop />
        <Hub />
        <Drop />
        <ul className="grid grid-cols-1 gap-2 min-[420px]:grid-cols-2">
          {tools.map((t) => (
            <ItemCard key={t.name} item={t} compact />
          ))}
        </ul>
      </div>
    </div>
  );
}
