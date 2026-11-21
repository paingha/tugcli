import Link from 'next/link';
import { type CSSProperties, Fragment, type ReactNode } from 'react';
import {
  ArrowRight,
  ArrowUpRight,
  Cloud,
  Container,
  FileCode,
  KeyRound,
  Laptop,
  Network,
  Radar,
  Server,
  ShieldCheck,
  Stethoscope,
  Unplug,
} from 'lucide-react';
import { CopyCommand } from '@/components/home/copy-command';
import { FlowDiagram } from '@/components/home/flow-diagram';
import { HeroStage } from '@/components/home/hero-stage';
import { LogoMark } from '@/components/logo';
import { repoUrl } from '@/lib/shared';

const install = 'go install github.com/paingha/tugcli/cmd/tug@latest';
const release = 'v0.1.0-alpha.1';

export default function HomePage() {
  return (
    <main className="flex flex-1 flex-col overflow-x-clip bg-fd-background">
      <Hero />
      <Providers />
      <Catalog />
      <Everyday />
      <Bridge />
      <Explains />
      <CastOff />
      <Footer />
    </main>
  );
}

/* ---------------------------------------------------------------- shared */

const container = 'mx-auto w-full max-w-6xl px-5 sm:px-8';

function PrimaryButton({ href, children }: { href: string; children: ReactNode }) {
  return (
    <Link
      href={href}
      className="group inline-flex items-center gap-1.5 rounded-full bg-fd-foreground px-5 py-2.5 text-[15px] font-semibold text-fd-background transition hover:opacity-85"
    >
      {children}
      <ArrowRight className="size-4 transition-transform group-hover:translate-x-0.5" />
    </Link>
  );
}

function SecondaryButton({ href, children }: { href: string; children: ReactNode }) {
  const external = href.startsWith('http');
  const className =
    'inline-flex items-center gap-1.5 rounded-full bg-fd-foreground/[0.06] px-5 py-2.5 text-[15px] font-semibold transition hover:bg-fd-foreground/[0.1]';
  return external ? (
    <a href={href} className={className}>
      {children}
    </a>
  ) : (
    <Link href={href} className={className}>
      {children}
    </Link>
  );
}

function TextLink({ href, children }: { href: string; children: ReactNode }) {
  return (
    <Link
      href={href}
      className="group inline-flex items-center gap-1 text-[15px] font-semibold text-(--color-tug) hover:underline"
    >
      {children}
      <ArrowRight className="size-4 transition-transform group-hover:translate-x-0.5" />
    </Link>
  );
}

function Title({ children, className = '' }: { children: ReactNode; className?: string }) {
  return (
    <h2
      className={`font-display text-[2.2rem] leading-[1.04] font-bold tracking-[-0.035em] text-balance sm:text-5xl lg:text-[3.5rem] ${className}`}
    >
      {children}
    </h2>
  );
}

function Code({ children }: { children: ReactNode }) {
  return <code className="rounded-md bg-fd-foreground/[0.06] px-1.5 py-0.5 font-mono text-[0.86em]">{children}</code>;
}

/* ------------------------------------------------------------------ hero */

/** Words that rise into place one after another. */
function Reveal({ words, start = 0 }: { words: string[]; start?: number }) {
  return (
    <>
      {words.map((w, i) => (
        <Fragment key={i}>
          <span className="inline-block animate-word-in" style={{ animationDelay: `${(start + i) * 90}ms` }}>
            {w}
          </span>
          {i < words.length - 1 ? ' ' : null}
        </Fragment>
      ))}
    </>
  );
}

function Hero() {
  return (
    <section className="pt-12 sm:pt-20">
      <div className={`${container} flex flex-col items-center text-center`}>
        <a
          href={`${repoUrl}/releases/tag/${release}`}
          className="group inline-flex animate-word-in items-center gap-2 rounded-full border bg-fd-background py-1 pr-3 pl-1 text-[13px] font-medium shadow-[0_1px_2px_rgba(15,23,42,0.05)] transition-colors hover:bg-fd-foreground/[0.03]"
        >
          <span className="rounded-full bg-(--color-tug)/12 px-2 py-0.5 text-[11.5px] font-semibold text-(--color-tug)">
            Alpha
          </span>
          tug {release} is out
          <ArrowUpRight className="size-3.5 text-fd-muted-foreground transition-transform group-hover:-translate-y-px group-hover:translate-x-px" />
        </a>

        <h1 className="mt-7 max-w-4xl font-display text-[2.9rem] leading-[1] font-bold tracking-[-0.045em] sm:text-7xl lg:text-[5.4rem]">
          <Reveal words={['Every', 'cluster,']} start={1} />{' '}
          <span className="whitespace-nowrap">
            <Reveal words={['one']} start={3} />{' '}
            <span className="relative inline-block animate-word-in text-(--color-tug)" style={{ animationDelay: '360ms' }}>
              tug
              <svg
                aria-hidden="true"
                viewBox="0 0 120 14"
                preserveAspectRatio="none"
                className="absolute -bottom-1.5 left-0 h-2.5 w-full text-(--color-harbour) sm:-bottom-2.5 sm:h-3.5"
              >
                <path
                  d="M3 8 Q17 1 32 8 T62 8 T92 8 T117 8"
                  fill="none"
                  stroke="currentColor"
                  strokeWidth="3.5"
                  strokeLinecap="round"
                  pathLength={1}
                  className="animate-draw"
                  style={{ strokeDasharray: 1, strokeDashoffset: 1, animationDelay: '900ms' }}
                />
              </svg>
            </span>{' '}
            <Reveal words={['away.']} start={5} />
          </span>
        </h1>

        <p
          className="mt-6 max-w-[36rem] animate-word-in text-[17px] leading-relaxed text-fd-muted-foreground sm:text-xl"
          style={{ animationDelay: '560ms' }}
        >
          tug finds your Kubernetes clusters, keeps their keys in your keychain, and gives every terminal a cluster of its
          own.
        </p>

        <div
          className="mt-8 flex animate-word-in flex-wrap items-center justify-center gap-3"
          style={{ animationDelay: '680ms' }}
        >
          <PrimaryButton href="/docs/getting-started/installation">Get started</PrimaryButton>
          <SecondaryButton href="/docs">Read the guide</SecondaryButton>
        </div>

        <div className="mt-6 w-full max-w-[33rem] animate-word-in" style={{ animationDelay: '800ms' }}>
          <CopyCommand command={install} className="rounded-full bg-fd-background pl-5" />
        </div>
      </div>

      <div className={`${container} mt-12 max-w-7xl animate-rise sm:mt-16 [animation-delay:350ms]`}>
        <HeroStage />
      </div>
    </section>
  );
}

/* ------------------------------------------------------------- providers */

const providerNames = [
  'kubeconfig',
  'kind',
  'k3d',
  'minikube',
  'Docker Desktop',
  'OrbStack',
  'Amazon EKS',
  'Google GKE',
  'Azure AKS',
  'Cluster API',
  'Argo CD',
  'DigitalOcean',
  'Akamai LKE',
  'Vultr',
  'CoreWeave',
  'Crusoe',
  'Nebius',
  'Rancher',
  'Teleport',
  'Tailscale',
];

function Providers() {
  const row = (copy: boolean) =>
    providerNames.map((p) => (
      <li
        key={`${p}-${copy}`}
        aria-hidden={copy || undefined}
        className={`shrink-0 font-display text-[19px] font-semibold tracking-[-0.01em] whitespace-nowrap text-fd-foreground/45 ${copy ? 'marquee-copy' : ''}`}
      >
        {p}
      </li>
    ));
  return (
    <section className="pt-16 pb-4 sm:pt-20 sm:pb-6">
      <p className={`${container} text-center text-[15px] text-fd-muted-foreground`}>
        Finds clusters in all the places you keep them, and{' '}
        <Link href="/docs/plugins/writing" className="font-medium text-fd-foreground underline-offset-4 hover:underline">
          anywhere else with a plugin
        </Link>
        .
      </p>
      <div className="marquee mx-auto mt-7 max-w-6xl overflow-hidden [mask-image:linear-gradient(to_right,transparent,black_12%,black_88%,transparent)]">
        <ul className="marquee-track flex w-max gap-x-12 px-6">
          {row(false)}
          {row(true)}
        </ul>
      </div>
    </section>
  );
}

/* --------------------------------------------------------------- catalog */

const tiles = [
  { icon: <FileCode />, tint: 'bg-[#e8efff] text-[#3166d6] dark:bg-[#3166d6]/20 dark:text-[#9dbbff]', pos: 'left-[3%] top-[6%]', size: 'size-16', tilt: '-8deg', delay: '0s' },
  { icon: <Laptop />, tint: 'bg-[#e3f6ec] text-[#1f8a55] dark:bg-[#1f8a55]/20 dark:text-[#7fe0ad]', pos: 'left-[15%] top-[44%]', size: 'size-14', tilt: '6deg', delay: '-2s' },
  { icon: <Container />, tint: 'bg-[#f1ebff] text-[#7347d1] dark:bg-[#7347d1]/25 dark:text-[#c3a9ff]', pos: 'left-[5%] top-[78%]', size: 'size-12', tilt: '-4deg', delay: '-4.5s' },
  { icon: <KeyRound />, tint: 'bg-[#fff6dc] text-[#b07a00] dark:bg-[#b07a00]/25 dark:text-[#ffd36b]', pos: 'left-[19%] top-[2%]', size: 'size-11', tilt: '10deg', delay: '-1s' },
  { icon: <Cloud />, tint: 'bg-[#fff0e6] text-[#d2601f] dark:bg-[#d2601f]/20 dark:text-[#ffb27d]', pos: 'right-[4%] top-[4%]', size: 'size-16', tilt: '7deg', delay: '-3s' },
  { icon: <Server />, tint: 'bg-[#e4f6f7] text-[#16808a] dark:bg-[#16808a]/25 dark:text-[#7fdbe2]', pos: 'right-[16%] top-[40%]', size: 'size-14', tilt: '-6deg', delay: '-5s' },
  { icon: <Network />, tint: 'bg-[#ffe9ec] text-[#c8344a] dark:bg-[#c8344a]/25 dark:text-[#ff9aa8]', pos: 'right-[5%] top-[76%]', size: 'size-12', tilt: '5deg', delay: '-1.5s' },
  { icon: <ShieldCheck />, tint: 'bg-[#eef0f3] text-[#33404f] dark:bg-white/10 dark:text-[#dbe6ef]', pos: 'right-[20%] top-[0%]', size: 'size-11', tilt: '-10deg', delay: '-6s' },
];

const catalogPoints = [
  {
    icon: <Radar />,
    title: 'Finds them for you',
    body: (
      <>
        <Code>tug import</Code> reads kubeconfig files, local tools such as kind and minikube, and your cloud accounts.{' '}
        <Code>tug sync</Code> keeps the list fresh.
      </>
    ),
  },
  {
    icon: <KeyRound />,
    title: 'Keys in your keychain',
    body: 'Tokens and certificates live in your operating system’s keychain. The kubeconfig files tug writes hold no secrets.',
  },
  {
    icon: <Unplug />,
    title: 'Hands off your kubeconfig',
    body: (
      <>
        tug never edits <Code>~/.kube/config</Code>. Each terminal gets its own kubeconfig instead, so trying tug is
        risk-free.
      </>
    ),
  },
];

function Catalog() {
  return (
    <section className="py-16 sm:py-24">
      <div className={`${container} reveal`}>
        <div className="relative">
          <div aria-hidden="true" className="pointer-events-none absolute inset-0 max-lg:hidden">
            {tiles.map((t, i) => (
              <span
                key={i}
                className={`bob absolute flex items-center justify-center rounded-2xl shadow-[0_12px_30px_-14px_rgba(15,23,42,0.35)] ring-1 ring-black/[0.04] dark:ring-white/10 [&_svg]:size-[45%] ${t.pos} ${t.size} ${t.tint}`}
                style={{ '--tilt': t.tilt, animationDelay: t.delay } as CSSProperties}
              >
                {t.icon}
              </span>
            ))}
          </div>
          <div aria-hidden="true" className="mb-8 flex justify-center gap-2.5 lg:hidden">
            {tiles.slice(0, 6).map((t, i) => (
              <span
                key={i}
                className={`flex size-11 items-center justify-center rounded-xl ring-1 ring-black/[0.04] dark:ring-white/10 [&_svg]:size-5 ${t.tint}`}
              >
                {t.icon}
              </span>
            ))}
          </div>
          <div className="relative mx-auto max-w-2xl text-center lg:py-14">
            <Title>Finds every cluster you can reach.</Title>
            <p className="mx-auto mt-5 max-w-xl text-[17px] leading-relaxed text-fd-muted-foreground sm:text-lg">
              Point tug at your kubeconfig files, local tools and cloud accounts. It builds one catalog of every cluster,
              so you never hunt for a context name again.
            </p>
            <div className="mt-6">
              <TextLink href="/docs/providers">See every provider</TextLink>
            </div>
          </div>
        </div>

        <div className="mt-14 grid grid-cols-1 gap-10 border-t pt-10 sm:mt-16 md:grid-cols-3 md:gap-8">
          {catalogPoints.map((p) => (
            <div key={p.title} className="min-w-0">
              <span className="text-fd-foreground [&_svg]:size-6">{p.icon}</span>
              <h3 className="mt-4 text-[17px] font-semibold tracking-tight">{p.title}</h3>
              <p className="mt-1.5 text-[15.5px] leading-relaxed text-fd-muted-foreground">{p.body}</p>
            </div>
          ))}
        </div>
      </div>
    </section>
  );
}

/* -------------------------------------------------------------- everyday */

function Window({ title, children, className = '' }: { title: string; children: ReactNode; className?: string }) {
  return (
    <div
      className={`min-w-0 overflow-hidden rounded-xl border border-white/10 bg-[#0b1724] text-left shadow-[0_24px_50px_-24px_rgba(3,12,24,0.75)] ${className}`}
    >
      <div className="flex items-center gap-1.5 border-b border-white/[0.07] px-3 py-2">
        <span className="size-2 rounded-full bg-white/20" />
        <span className="size-2 rounded-full bg-white/20" />
        <span className="size-2 rounded-full bg-white/20" />
        <span className="ml-1.5 font-mono text-[10px] text-[#7b8fa3]">{title}</span>
      </div>
      <div className="px-3 py-2.5 font-mono text-[11px] leading-[1.75] text-[#dbe6ef]">{children}</div>
    </div>
  );
}

function SessionsArt() {
  return (
    <div className="relative mx-auto w-full max-w-[19rem]">
      <Window title="Terminal 1" className="w-[88%]">
        <div className="truncate">
          <span className="text-[#5fd4dc]">⎈ staging-us:default</span> <span className="text-[#7b8fa3]">$</span> tug ns pay
        </div>
        <div className="truncate text-[#8ea3b5]">namespace payments on staging-us</div>
      </Window>
      <Window title="Terminal 2" className="-mt-3 ml-auto w-[88%]">
        <div className="truncate">
          <span className="font-bold text-[#ff7b86]">⎈ prod-eu:default (read-only)</span>
        </div>
        <div className="truncate">
          <span className="text-[#7b8fa3]">$</span> kubectl get ns
        </div>
        <div className="truncate text-[#8ea3b5]">NAME&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;STATUS&nbsp;&nbsp;&nbsp;AGE</div>
      </Window>
    </div>
  );
}

function SafetyArt() {
  const rows = [
    { text: '⎈ staging-us:default', label: 'read-write', colour: 'text-[#5fd4dc]' },
    { text: '⎈ prod-eu:payments (read-only)', label: 'read-only', colour: 'font-bold text-[#ff7b86]' },
    { text: '⎈ prod-eu:payments (unlocked)', label: 'unlock 15m', colour: 'font-bold text-[#ffcf5c]' },
  ];
  return (
    <div className="mx-auto w-full max-w-[19rem] rounded-xl border border-white/10 bg-[#0b1724] px-3.5 py-1 shadow-[0_24px_50px_-24px_rgba(3,12,24,0.75)]">
      {rows.map((r) => (
        <div key={r.label} className="flex items-center justify-between gap-3 border-b border-white/[0.07] py-2.5 last:border-b-0">
          <span className={`min-w-0 truncate font-mono text-[11px] ${r.colour}`}>{r.text}</span>
          <span className="shrink-0 text-[9.5px] font-semibold tracking-wider text-[#7b8fa3] uppercase">{r.label}</span>
        </div>
      ))}
    </div>
  );
}

function ShortcutsArt() {
  const rows = [
    ['tug logs api -f', 'every pod, new ones too'],
    ['tug sh api', 'a shell in one of its pods'],
    ['tug pf api 8080', 'localhost:8080 to it'],
    ['tug ns pay', 'payments, in this terminal'],
  ];
  return (
    <ul className="mx-auto w-full max-w-[19rem] divide-y overflow-hidden rounded-xl border bg-fd-background text-left shadow-[0_24px_50px_-28px_rgba(15,23,42,0.45)]">
      {rows.map(([cmd, does]) => (
        <li key={cmd} className="px-3.5 py-2.5">
          <code className="block font-mono text-[12px] font-semibold text-(--color-tug)">{cmd}</code>
          <span className="block truncate text-[12px] text-fd-muted-foreground">{does}</span>
        </li>
      ))}
    </ul>
  );
}

const everyday = [
  {
    art: <SessionsArt />,
    panel: 'bg-[linear-gradient(160deg,#dff4f3,#eef7f0)] dark:bg-[linear-gradient(160deg,#0f2a30,#0f2420)]',
    title: 'A cluster per terminal',
    body: 'Each terminal remembers its own cluster and namespace. kubectl, helm and k9s follow along, and switching one window never moves another.',
    href: '/docs/concepts/sessions',
  },
  {
    art: <SafetyArt />,
    panel: 'bg-[linear-gradient(160deg,#ffe8e6,#fff3ea)] dark:bg-[linear-gradient(160deg,#341a20,#2c1d18)]',
    title: 'Careful with production',
    body: 'Clusters labelled env=prod open read-only. tug asks before anything that could change them, until you unlock writes for a while.',
    href: '/docs/concepts/read-only',
  },
  {
    art: <ShortcutsArt />,
    panel: 'bg-[linear-gradient(160deg,#fff3d6,#fdf7ea)] dark:bg-[linear-gradient(160deg,#30260f,#272012)]',
    title: 'Type a few letters',
    body: 'Names are fuzzy, so api finds deploy/api-server. When two match equally well, tug shows a list and you pick.',
    href: '/docs/concepts/targets',
  },
];

function Everyday() {
  return (
    <section className="py-16 sm:py-24">
      <div className={`${container} reveal`}>
        <div className="grid grid-cols-1 gap-6 lg:grid-cols-[minmax(0,1.3fr)_minmax(0,1fr)] lg:items-end">
          <Title>Less typing. Fewer accidents.</Title>
          <p className="max-w-md text-[17px] leading-relaxed text-fd-muted-foreground lg:justify-self-end">
            The busywork around Kubernetes, handled: switching clusters, finding pods, and not breaking production on a
            Friday afternoon.
          </p>
        </div>

        <div className="mt-12 grid grid-cols-1 gap-8 md:grid-cols-3 md:gap-5">
          {everyday.map((e) => (
            <Link key={e.title} href={e.href} className="group min-w-0">
              <div
                className={`flex aspect-[5/4] items-center justify-center overflow-hidden rounded-3xl px-5 transition-transform duration-300 group-hover:-translate-y-1 ${e.panel}`}
              >
                {e.art}
              </div>
              <h3 className="mt-5 flex items-center gap-1.5 text-[17px] font-semibold tracking-tight">
                {e.title}
                <ArrowRight className="size-4 -translate-x-1 opacity-0 transition group-hover:translate-x-0 group-hover:opacity-100" />
              </h3>
              <p className="mt-1.5 text-[15.5px] leading-relaxed text-fd-muted-foreground">{e.body}</p>
            </Link>
          ))}
        </div>
      </div>
    </section>
  );
}

/* ---------------------------------------------------------------- bridge */

function Bridge() {
  return (
    <section className="py-16 sm:py-24">
      <div className={`${container} reveal`}>
        <div className="mx-auto max-w-2xl text-center">
          <Title>Any cluster in. Every tool out.</Title>
          <p className="mx-auto mt-5 max-w-xl text-[17px] leading-relaxed text-fd-muted-foreground sm:text-lg">
            Providers bring clusters in from wherever they live. Your tools pick up the session without a single flag,
            because tug sets <Code>KUBECONFIG</Code> for that terminal only.
          </p>
        </div>

        <div className="mx-auto mt-12 max-w-5xl rounded-[28px] border bg-fd-card/60 p-4 sm:mt-16 sm:p-8 lg:p-10">
          <FlowDiagram />
        </div>

        <div className="mt-8 flex flex-wrap justify-center gap-x-8 gap-y-3">
          <TextLink href="/docs/providers">Browse providers</TextLink>
          <TextLink href="/docs/plugins/writing">Write a plugin</TextLink>
        </div>
      </div>
    </section>
  );
}

/* -------------------------------------------------------------- explains */

function Explains() {
  return (
    <section className="py-10 sm:py-16">
      <div className={`${container} reveal`}>
        <div className="relative isolate overflow-hidden rounded-[28px] bg-[#0b1724] text-[#eef3f7] sm:rounded-[36px]">
          <div
            aria-hidden="true"
            className="pointer-events-none absolute -top-32 -right-24 -z-10 size-[30rem] rounded-full bg-[radial-gradient(closest-side,rgba(95,212,220,0.18),transparent)]"
          />
          <div
            aria-hidden="true"
            className="pointer-events-none absolute -bottom-40 -left-24 -z-10 size-[30rem] rounded-full bg-[radial-gradient(closest-side,rgba(255,122,71,0.16),transparent)]"
          />
          <div className="grid grid-cols-1 gap-10 p-6 sm:p-10 lg:grid-cols-[minmax(0,0.78fr)_minmax(0,1.22fr)] lg:items-center lg:gap-12 lg:p-14">
            <div className="min-w-0">
              <span className="inline-flex size-11 items-center justify-center rounded-2xl bg-white/10 text-[#ffb38a]">
                <Stethoscope className="size-5" />
              </span>
              <h2 className="mt-6 font-display text-[2rem] leading-[1.05] font-bold tracking-[-0.03em] text-balance sm:text-[2.75rem]">
                When something breaks, tug tells you why.
              </h2>
              <p className="mt-5 text-[17px] leading-relaxed text-[#a9bccb]">
                <code className="font-mono text-[0.9em] text-[#dbe6ef]">tug why</code> reads a workload, its pods and
                their events, and explains what&apos;s wrong in plain words.{' '}
                <code className="font-mono text-[0.9em] text-[#dbe6ef]">tug doctor</code> checks every step between you
                and a cluster. Both end with the next thing to try.
              </p>
              <div className="mt-7 flex flex-wrap gap-x-7 gap-y-3 text-[15px] font-semibold">
                <Link href="/docs/guides/workloads" className="group inline-flex items-center gap-1 text-[#ffb38a] hover:underline">
                  Debug a workload <ArrowRight className="size-4 transition-transform group-hover:translate-x-0.5" />
                </Link>
                <Link href="/docs/guides/doctor" className="group inline-flex items-center gap-1 text-[#ffb38a] hover:underline">
                  Run the doctor <ArrowRight className="size-4 transition-transform group-hover:translate-x-0.5" />
                </Link>
              </div>
            </div>

            <div className="min-w-0 overflow-hidden rounded-2xl border border-white/10 bg-[#060e18] shadow-[0_40px_80px_-40px_rgba(0,0,0,0.9)]">
              <div className="flex items-center gap-1.5 border-b border-white/[0.07] px-4 py-3">
                <span className="size-2.5 rounded-full bg-white/15" />
                <span className="size-2.5 rounded-full bg-white/15" />
                <span className="size-2.5 rounded-full bg-white/15" />
                <span className="ml-2 font-mono text-[11px] text-[#7b8fa3]">⎈ shop-eu:shop</span>
              </div>
              <div className="px-4 py-4 font-mono text-[11px] leading-[1.75] text-[#dbe6ef] sm:px-5 sm:text-[11.5px]">
                <p>
                  <span className="text-[#7b8fa3]">$</span> tug why billing
                </p>
                <p className="font-bold text-white">deploy/billing in shop is not ready: 0/1 ready</p>
                <div className="mt-3 overflow-x-auto whitespace-pre text-[#a9bccb] [scrollbar-width:thin]">
                  {'POD                  STATUS             READY   RESTARTS   NODE     AGE\n'}
                  {'billing-5d4f-x2x4z   '}
                  <span className="text-[#ff7b86]">CrashLoopBackOff</span>
                  {'   0/1     7          node-1   3h'}
                </div>
                <div className="mt-3 space-y-0.5 [&>p]:pl-[9ch] [&>p]:-indent-[9ch] [&>p]:whitespace-pre-wrap">
                  <p>
                    <span className="font-bold text-[#ff7b86]">error</span>
                    {'    pod/billing-5d4f-x2x4z, container main: CrashLoopBackOff'}
                  </p>
                  <p className="text-[#a9bccb]">{'         keeps crashing and waits to restart; last exit code 1 (Error)'}</p>
                  <p>
                    {'         '}
                    <span className="text-[#5ee3a1]">next:</span> read the crashed container&apos;s logs:{' '}
                    <span className="text-[#ffcf5c]">`tug logs pod/billing-5d4f-x2x4z --previous -n shop`</span>
                  </p>
                </div>
              </div>
            </div>
          </div>
        </div>
      </div>
    </section>
  );
}

/* -------------------------------------------------------------- cast off */

function CastOff() {
  const steps = [
    { title: 'Install tug', cmd: install },
    { title: 'Add a line to ~/.zshrc', cmd: 'eval "$(tug init zsh)"' },
    { title: 'Find your clusters', cmd: 'tug import' },
  ];
  return (
    <section className="py-20 sm:py-32">
      <div className={`${container} reveal flex flex-col items-center text-center`}>
        <LogoMark className="size-14 text-fd-foreground" />
        <Title className="mt-6">Ready to cast off?</Title>
        <p className="mt-5 max-w-lg text-[17px] leading-relaxed text-fd-muted-foreground sm:text-lg">
          Three steps, about two minutes. tug never touches your kubeconfig, so there&apos;s nothing to undo.
        </p>
        <div className="mt-8 flex flex-wrap justify-center gap-3">
          <PrimaryButton href="/docs/getting-started/quick-start">Quick start</PrimaryButton>
          <SecondaryButton href={repoUrl}>View on GitHub</SecondaryButton>
        </div>

        <ol className="mt-14 grid w-full max-w-xl grid-cols-1 gap-3 text-left">
          {steps.map((s, i) => (
            <li key={s.title} className="min-w-0 rounded-2xl border bg-fd-card/60 p-4">
              <p className="mb-3 flex items-center gap-2.5 text-[14px] font-semibold">
                <span className="flex size-6 items-center justify-center rounded-full bg-fd-foreground text-[12px] font-bold text-fd-background">
                  {i + 1}
                </span>
                {s.title}
              </p>
              <CopyCommand command={s.cmd} className="bg-fd-background" />
            </li>
          ))}
        </ol>
      </div>
    </section>
  );
}

/* ---------------------------------------------------------------- footer */

const footerColumns: { title: string; links: [string, string][] }[] = [
  {
    title: 'Start',
    links: [
      ['Installation', '/docs/getting-started/installation'],
      ['Shell setup', '/docs/getting-started/shell-setup'],
      ['Quick start', '/docs/getting-started/quick-start'],
      ['FAQ', '/docs/faq'],
    ],
  },
  {
    title: 'Guides',
    links: [
      ['Workloads', '/docs/guides/workloads'],
      ['Private clusters', '/docs/guides/tunnels'],
      ['Doctor', '/docs/guides/doctor'],
      ['Scripting', '/docs/guides/scripting'],
    ],
  },
  {
    title: 'Reference',
    links: [
      ['Commands', '/docs/reference/commands'],
      ['Configuration', '/docs/reference/configuration'],
      ['Providers', '/docs/providers'],
      ['Plugins', '/docs/plugins'],
    ],
  },
  {
    title: 'Project',
    links: [
      ['GitHub', repoUrl],
      ['Releases', `${repoUrl}/releases`],
      ['Contributing', '/docs/contributing'],
      ['Licence', `${repoUrl}/blob/main/LICENSE`],
    ],
  },
];

function Footer() {
  return (
    <footer className="border-t">
      <div className={`${container} grid grid-cols-2 gap-10 py-14 md:grid-cols-[minmax(0,1.4fr)_repeat(4,minmax(0,1fr))]`}>
        <div className="col-span-2 md:col-span-1">
          <p className="inline-flex items-center gap-2 font-display text-lg font-bold tracking-tight">
            <LogoMark className="size-7 text-fd-foreground" /> tug
          </p>
          <p className="mt-4 max-w-[16rem] text-[14.5px] leading-relaxed text-fd-muted-foreground">
            Kubernetes clusters, one tug away. For people who live in a terminal.
          </p>
        </div>
        {footerColumns.map((c) => (
          <nav key={c.title} aria-label={c.title} className="min-w-0">
            <p className="text-[13px] font-semibold text-fd-muted-foreground">{c.title}</p>
            <ul className="mt-3 space-y-2.5">
              {c.links.map(([label, href]) => (
                <li key={label}>
                  {href.startsWith('http') ? (
                    <a href={href} className="text-[14.5px] hover:text-(--color-tug)">
                      {label}
                    </a>
                  ) : (
                    <Link href={href} className="text-[14.5px] hover:text-(--color-tug)">
                      {label}
                    </Link>
                  )}
                </li>
              ))}
            </ul>
          </nav>
        ))}
      </div>
      <div className={`${container} flex flex-col gap-2 border-t py-6 text-[13px] text-fd-muted-foreground sm:flex-row sm:justify-between`}>
        <p>MIT licensed. © 2026 The tug authors.</p>
        <p>
          Latest release:{' '}
          <a href={`${repoUrl}/releases/tag/${release}`} className="font-mono hover:text-fd-foreground">
            {release}
          </a>
        </p>
      </div>
    </footer>
  );
}
