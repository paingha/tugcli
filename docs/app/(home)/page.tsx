import Link from 'next/link';
import type { ReactNode } from 'react';
import { ArrowRight, BookOpen, KeyRound, Puzzle, ShieldCheck, Stethoscope, Terminal } from 'lucide-react';
import { CopyCommand } from '@/components/home/copy-command';
import { HeroStage } from '@/components/home/hero-stage';
import { Stars } from '@/components/home/night-scene';
import { LogoMark } from '@/components/logo';
import { repoUrl } from '@/lib/shared';

const install = 'go install github.com/paingha/tugcli/cmd/tug@latest';

export default function HomePage() {
  return (
    <main className="flex flex-1 flex-col">
      <Hero />
      <WorksWith />
      <Sessions />
      <Bento />
      <CastOff />
      <Footer />
    </main>
  );
}

/* ------------------------------------------------------------------ hero */

function Hero() {
  return (
    <section className="sm:px-4 sm:pt-4">
      <div className="relative isolate overflow-hidden bg-[linear-gradient(180deg,#0b1b2e_0%,#0c2034_55%,#0d2338_100%)] text-[#eef3f7] ring-1 ring-white/[0.06] sm:rounded-[2rem]">
        <Stars className="pointer-events-none absolute inset-0 -z-10 h-full w-full" />
        <div className="pointer-events-none absolute -top-40 left-1/2 -z-10 h-80 w-[60rem] -translate-x-1/2 rounded-full bg-[#ff7a47]/[0.07] blur-3xl motion-safe:animate-[aurora_14s_ease-in-out_infinite]" />
        <HeroStage copy={<HeroCopy />} />
      </div>
    </section>
  );
}

/** Words that rise into place one after another. */
function Reveal({ words, start = 0 }: { words: string[]; start?: number }) {
  return (
    <>
      {words.map((w, i) => (
        <span
          key={i}
          className="inline-block animate-word-in"
          style={{ animationDelay: `${(start + i) * 90}ms` }}
        >
          {w}
          {i < words.length - 1 ? ' ' : ''}
        </span>
      ))}
    </>
  );
}

function HeroCopy() {
  return (
    <>
      <p className="inline-flex animate-word-in items-center gap-2 text-[13px] font-medium text-[#8fdde3]">
        <LogoMark className="size-5 text-[#eef3f7]" />
        tug, for Kubernetes
      </p>
      <h1 className="mt-5 font-display text-[2.75rem] leading-[1.02] font-extrabold tracking-[-0.04em] sm:text-6xl lg:text-[4.4rem]">
        <Reveal words={['Every', 'cluster,']} start={1} />
        <br />
        <Reveal words={['one']} start={3} />{' '}
        <span className="relative inline-block animate-word-in text-[#ff7a47]" style={{ animationDelay: '360ms' }}>
          tug
          <svg
            aria-hidden="true"
            viewBox="0 0 120 14"
            preserveAspectRatio="none"
            className="absolute -bottom-1.5 left-0 h-2.5 w-full text-[#5fd4dc] sm:-bottom-2 sm:h-3"
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
      </h1>
      <p
        className="mt-6 max-w-[34rem] animate-word-in text-[17px] leading-relaxed text-[#a9bccb] sm:text-lg"
        style={{ animationDelay: '560ms' }}
      >
        tug finds your Kubernetes clusters, keeps their keys in your keychain, and gives every terminal window a
        cluster of its own. Your <code className="font-mono text-[0.9em] text-[#dbe6ef]">~/.kube/config</code> is
        never changed.
      </p>
      <div className="mt-8 flex animate-word-in flex-wrap items-center gap-3" style={{ animationDelay: '680ms' }}>
        <Link
          href="/docs/getting-started/installation"
          className="group inline-flex items-center gap-2 rounded-full bg-[#ff7a47] px-5 py-2.5 text-sm font-semibold text-[#0b1724] shadow-[0_10px_30px_-10px_rgba(255,122,71,0.7)] transition hover:bg-[#ff8c5e] hover:shadow-[0_14px_36px_-10px_rgba(255,122,71,0.9)]"
        >
          Get started <ArrowRight className="size-4 transition-transform group-hover:translate-x-0.5" />
        </Link>
        <Link
          href="/docs"
          className="inline-flex items-center gap-2 rounded-full px-4 py-2.5 text-sm font-semibold text-[#dbe6ef] ring-1 ring-white/15 transition hover:bg-white/[0.06] hover:ring-white/25"
        >
          <BookOpen className="size-4" /> Read the guide
        </Link>
      </div>
      <div className="mt-8 max-w-[34rem] animate-word-in" style={{ animationDelay: '800ms' }}>
        <CopyCommand command={install} variant="night" />
      </div>
    </>
  );
}

/* ------------------------------------------------------------ works with */

const providerNames = [
  'kubeconfig',
  'kind',
  'k3d',
  'minikube',
  'Docker Desktop',
  'OrbStack',
  'Rancher Desktop',
  'Amazon EKS',
  'Google GKE',
  'Akamai LKE',
  'CoreWeave CKS',
];

function WorksWith() {
  return (
    <section className="mx-auto w-full max-w-6xl px-5 py-12 sm:px-10">
      <p className="text-center text-xs font-semibold uppercase tracking-[0.16em] text-fd-muted-foreground">
        Finds clusters in
      </p>
      <ul className="mx-auto mt-5 flex max-w-4xl flex-wrap justify-center gap-x-5 gap-y-2.5 text-[14.5px] font-semibold text-fd-foreground/65 sm:gap-x-7">
        {providerNames.map((p) => (
          <li key={p} className="whitespace-nowrap">
            {p}
          </li>
        ))}
        <li className="whitespace-nowrap">
          <Link href="/docs/plugins/writing" className="text-(--color-tug) hover:underline">
            + your own
          </Link>
        </li>
      </ul>
    </section>
  );
}

/* -------------------------------------------------------------- sessions */

function Eyebrow({ children }: { children: ReactNode }) {
  return <p className="text-xs font-bold uppercase tracking-[0.16em] text-(--color-tug)">{children}</p>;
}

function Heading({ children }: { children: ReactNode }) {
  return (
    <h2 className="mt-3 font-display text-[2rem] leading-[1.08] font-extrabold tracking-[-0.03em] sm:text-[2.6rem]">
      {children}
    </h2>
  );
}

type Row = { cmd?: string; out?: string; tone?: 'dim' | 'err' };

function MiniTerminal({ title, prompt, readOnly, rows }: { title: string; prompt: string; readOnly?: boolean; rows: Row[] }) {
  return (
    <div className="min-w-0 overflow-hidden rounded-xl border border-white/10 bg-[#0b1724] shadow-[0_24px_60px_-28px_rgba(3,12,24,0.7)]">
      <div className="flex items-center gap-1.5 border-b border-white/[0.07] px-3.5 py-2.5">
        <span className="size-2 rounded-full bg-white/20" />
        <span className="size-2 rounded-full bg-white/20" />
        <span className="size-2 rounded-full bg-white/20" />
        <span className="ml-2 font-mono text-[10.5px] text-[#7b8fa3]">{title}</span>
      </div>
      <div className="overflow-x-auto px-3.5 py-3 font-mono text-[11.5px] leading-[1.75] text-[#dbe6ef] sm:text-[12px]">
        {rows.map((r, i) =>
          r.cmd !== undefined ? (
            <div key={i} className="whitespace-nowrap">
              <span className={readOnly ? 'font-bold text-[#ff7b86]' : 'text-[#5fd4dc]'}>{prompt}</span>{' '}
              <span className="text-[#7b8fa3]">$</span> {r.cmd}
            </div>
          ) : (
            <div
              key={i}
              className={`whitespace-pre ${r.tone === 'dim' ? 'text-[#8ea3b5]' : r.tone === 'err' ? 'text-[#ff7b86]' : ''}`}
            >
              {r.out}
            </div>
          ),
        )}
      </div>
    </div>
  );
}

function Sessions() {
  const points = [
    ['Switching one terminal never moves another.', 'No more shared “current context”.'],
    ['kubectl, helm and k9s follow along.', 'tug sets KUBECONFIG for that window only.'],
    ['Your prompt shows where you are.', 'Red means production, so you notice.'],
  ];
  return (
    <section className="border-t bg-fd-card/40">
      <div className="mx-auto grid w-full max-w-6xl grid-cols-1 gap-12 px-5 py-20 sm:px-10 sm:py-28 lg:grid-cols-[minmax(0,0.9fr)_minmax(0,1.1fr)] lg:items-center lg:gap-16">
        <div className="min-w-0">
          <Eyebrow>Sessions</Eyebrow>
          <Heading>Two terminals. Two clusters. No surprises.</Heading>
          <p className="mt-5 text-[17px] leading-relaxed text-fd-muted-foreground">
            Each terminal window remembers its own cluster and namespace. Check staging in one while you watch
            production in the other.
          </p>
          <ul className="mt-8 space-y-4">
            {points.map(([title, body]) => (
              <li key={title} className="flex gap-3">
                <span className="mt-2 size-1.5 shrink-0 rounded-full bg-(--color-tug)" />
                <p className="text-[15px] leading-relaxed">
                  <span className="font-semibold">{title}</span>{' '}
                  <span className="text-fd-muted-foreground">{body}</span>
                </p>
              </li>
            ))}
          </ul>
          <Link
            href="/docs/concepts/sessions"
            className="mt-8 inline-flex items-center gap-1.5 text-sm font-semibold text-(--color-tug) hover:underline"
          >
            How sessions work <ArrowRight className="size-4" />
          </Link>
        </div>
        <div className="relative min-w-0">
          <div className="grid gap-4 lg:pr-10">
            <MiniTerminal
              title="Terminal 1"
              prompt="⎈ staging-us:default"
              rows={[
                { cmd: 'kubectl get deploy' },
                { out: 'NAME     READY   AVAILABLE' },
                { out: 'api      3/3     3' },
                { out: 'worker   2/2     2' },
                { cmd: 'tug scale worker 4' },
                { out: 'Scaled deploy/worker to 4' },
              ]}
            />
          </div>
          <div className="mt-4 grid gap-4 lg:ml-10">
            <MiniTerminal
              title="Terminal 2"
              prompt="⎈ prod-eu:payments (read-only)"
              readOnly
              rows={[
                { cmd: 'tug scale ledger 0' },
                { out: 'prod-eu is read-only. Allow PATCH deployments/ledger/scale? [y/N] n', tone: 'dim' },
                { out: 'tug: refused PATCH deployments/ledger/scale: prod-eu is read-only; …', tone: 'err' },
                { cmd: 'tug unlock 15m' },
                { out: 'Unlocked: writes allowed on prod-eu until 3:45PM' },
              ]}
            />
          </div>
        </div>
      </div>
    </section>
  );
}

/* ----------------------------------------------------------------- bento */

function Card({ className = '', children }: { className?: string; children: ReactNode }) {
  return <div className={`min-w-0 overflow-hidden rounded-2xl border bg-fd-card p-6 sm:p-7 ${className}`}>{children}</div>;
}

function CardTitle({ icon, title, children }: { icon: ReactNode; title: string; children: ReactNode }) {
  return (
    <div>
      <div className="flex items-center gap-2.5">
        <span className="text-(--color-tug) [&_svg]:size-[18px]">{icon}</span>
        <h3 className="font-display text-lg font-bold tracking-tight">{title}</h3>
      </div>
      <p className="mt-2 text-[15px] leading-relaxed text-fd-muted-foreground">{children}</p>
    </div>
  );
}

function PromptLine({ tone, text, label }: { tone: 'normal' | 'ro' | 'un'; text: string; label: string }) {
  const colour = tone === 'normal' ? 'text-[#5fd4dc]' : tone === 'ro' ? 'font-bold text-[#ff7b86]' : 'font-bold text-[#ffcf5c]';
  return (
    <div className="flex items-center justify-between gap-3 border-b border-white/[0.06] py-2.5 last:border-b-0">
      <span className={`min-w-0 truncate font-mono text-[12px] ${colour}`}>{text}</span>
      <span className="shrink-0 text-[11px] font-medium uppercase tracking-wider text-[#7b8fa3]">{label}</span>
    </div>
  );
}

const shortcuts = [
  { tug: 'tug logs api -f', does: 'follow every pod of deploy/api-server, new pods too' },
  { tug: 'tug sh api', does: 'shell into one of its pods (bash, else sh)' },
  { tug: 'tug pf api 8080', does: 'forward localhost:8080 to it' },
  { tug: 'tug ns pay', does: 'switch to the payments namespace, in this terminal only' },
];

function Bento() {
  return (
    <section className="mx-auto w-full max-w-6xl px-5 py-20 sm:px-10 sm:py-28">
      <div className="max-w-2xl">
        <Eyebrow>What you get</Eyebrow>
        <Heading>The busywork around Kubernetes, handled.</Heading>
      </div>

      <div className="mt-12 grid grid-cols-1 gap-4 md:grid-cols-6">
        <Card className="md:col-span-4">
          <CardTitle icon={<ShieldCheck />} title="Careful with production">
            Clusters labelled <code className="font-mono text-[0.9em]">env=prod</code> open read-only. tug asks before
            anything that could change them, until you unlock writes for a while.
          </CardTitle>
          <div className="mt-6 rounded-xl bg-[#0b1724] px-4 py-1.5">
            <PromptLine tone="normal" text="⎈ staging-us:default" label="read-write" />
            <PromptLine tone="ro" text="⎈ prod-eu:payments (read-only)" label="read-only" />
            <PromptLine tone="un" text="⎈ prod-eu:payments (unlocked)" label="tug unlock 15m" />
          </div>
        </Card>

        <Card className="md:col-span-2">
          <CardTitle icon={<KeyRound />} title="Keys in your keychain">
            Tokens move out of kubeconfig files and into your OS keychain. The files tug writes hold no secrets.
          </CardTitle>
          <ol className="mt-6 space-y-2 font-mono text-[12px]">
            {['kubectl', 'tug auth token', 'keychain'].map((step, i) => (
              <li key={step} className="flex items-center gap-2.5">
                <span className="flex size-5 shrink-0 items-center justify-center rounded-full bg-(--color-tug)/12 text-[10px] font-bold text-(--color-tug)">
                  {i + 1}
                </span>
                <span className="rounded-md border bg-fd-background px-2 py-1">{step}</span>
              </li>
            ))}
          </ol>
        </Card>

        <Card className="md:col-span-6">
          <div className="grid grid-cols-1 gap-8 lg:grid-cols-[minmax(0,0.8fr)_minmax(0,1.2fr)] lg:items-center">
            <CardTitle icon={<Terminal />} title="Type a few letters">
              Names are fuzzy: <code className="font-mono text-[0.9em]">api</code> finds{' '}
              <code className="font-mono text-[0.9em]">deploy/api-server</code>. When two names match equally well,
              tug shows a list and you pick.
            </CardTitle>
            <ul className="divide-y rounded-xl border bg-fd-background">
              {shortcuts.map((s) => (
                <li key={s.tug} className="flex flex-col gap-1 px-4 py-3 sm:flex-row sm:items-center sm:gap-4">
                  <code className="shrink-0 font-mono text-[13px] font-semibold text-(--color-tug) sm:w-36">{s.tug}</code>
                  <span className="text-[14px] text-fd-muted-foreground">{s.does}</span>
                </li>
              ))}
            </ul>
          </div>
        </Card>

        <Card className="md:col-span-2">
          <CardTitle icon={<Stethoscope />} title="Explains what's wrong">
            <code className="font-mono text-[0.9em]">tug doctor</code> checks DNS, TLS, credentials, the clock and your
            permissions, then says how to fix what fails.
          </CardTitle>
        </Card>
        <Card className="md:col-span-2">
          <CardTitle icon={<Puzzle />} title="Add any provider">
            A plugin is a small program, in any language, that speaks JSON. Scaffold, test and package one in minutes.
          </CardTitle>
        </Card>
        <Card className="md:col-span-2">
          <CardTitle icon={<Terminal />} title="Everything else is kubectl">
            Commands tug doesn&apos;t have run as kubectl in your session, so{' '}
            <code className="font-mono text-[0.9em]">tug get pods</code> just works.
          </CardTitle>
        </Card>
      </div>
    </section>
  );
}

/* -------------------------------------------------------------- cast off */

function CastOff() {
  const steps = [
    { title: 'Install tug', cmd: install },
    { title: 'Add this line to ~/.zshrc', cmd: 'eval "$(tug init zsh)"' },
    { title: 'Find your clusters', cmd: 'tug import' },
  ];
  return (
    <section className="sm:px-4 sm:pb-4">
      <div className="relative isolate overflow-hidden bg-[#0b1b2e] text-[#eef3f7] ring-1 ring-white/[0.06] sm:rounded-[2rem]">
        <Stars className="pointer-events-none absolute inset-0 -z-10 h-full w-full opacity-70" />
        <div className="mx-auto grid w-full max-w-6xl grid-cols-1 gap-10 px-5 py-16 sm:px-10 sm:py-20 lg:grid-cols-[minmax(0,0.9fr)_minmax(0,1.1fr)] lg:items-center">
          <div className="min-w-0">
            <h2 className="font-display text-[2.2rem] leading-[1.05] font-extrabold tracking-[-0.03em] sm:text-5xl">
              Ready to cast off?
            </h2>
            <p className="mt-4 max-w-md text-[17px] leading-relaxed text-[#a9bccb]">
              Three steps, about two minutes. tug never edits your kubeconfig, so trying it is risk-free.
            </p>
            <div className="mt-8 flex flex-wrap gap-3">
              <Link
                href="/docs/getting-started/quick-start"
                className="inline-flex items-center gap-2 rounded-full bg-[#ff7a47] px-5 py-2.5 text-sm font-semibold text-[#0b1724] transition hover:bg-[#ff8c5e]"
              >
                Quick start <ArrowRight className="size-4" />
              </Link>
              <a
                href={repoUrl}
                className="inline-flex items-center rounded-full px-5 py-2.5 text-sm font-semibold ring-1 ring-white/15 transition hover:bg-white/[0.06]"
              >
                GitHub
              </a>
            </div>
          </div>
          <ol className="min-w-0 space-y-4">
            {steps.map((s, i) => (
              <li key={s.title} className="min-w-0">
                <p className="mb-2 flex items-center gap-2.5 text-sm font-semibold">
                  <span className="flex size-6 items-center justify-center rounded-full bg-[#ff7a47] font-display text-xs font-bold text-[#0b1724]">
                    {i + 1}
                  </span>
                  {s.title}
                </p>
                <CopyCommand command={s.cmd} variant="night" />
              </li>
            ))}
          </ol>
        </div>
      </div>
    </section>
  );
}

function Footer() {
  return (
    <footer className="py-10">
      <div className="mx-auto flex w-full max-w-6xl flex-col items-center justify-between gap-4 px-5 text-sm text-fd-muted-foreground sm:flex-row sm:px-10">
        <p className="inline-flex items-center gap-2">
          <LogoMark className="size-5 text-fd-foreground" /> tug, for people who live in a terminal. MIT licensed.
        </p>
        <nav className="flex flex-wrap justify-center gap-5">
          <Link href="/docs" className="hover:text-fd-foreground">
            Guide
          </Link>
          <Link href="/docs/reference/commands" className="hover:text-fd-foreground">
            Commands
          </Link>
          <Link href="/docs/faq" className="hover:text-fd-foreground">
            FAQ
          </Link>
          <Link href="/docs/contributing" className="hover:text-fd-foreground">
            Contributing
          </Link>
          <a href={repoUrl} className="hover:text-fd-foreground">
            GitHub
          </a>
        </nav>
      </div>
    </footer>
  );
}
