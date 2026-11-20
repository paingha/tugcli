import type { Metadata } from 'next';
import Link from 'next/link';
import { ArrowRight } from 'lucide-react';
import { HomeLayout } from 'fumadocs-ui/layouts/home';
import { FullSearchTrigger } from 'fumadocs-ui/layouts/shared/slots/search-trigger';
import { NightScene, Stars } from '@/components/home/night-scene';
import { baseOptions } from '@/lib/layout.shared';

export const metadata: Metadata = {
  title: 'Page not found',
  robots: { index: false },
};

const links = [
  { href: '/docs', title: 'The guide', note: 'Start from the top' },
  { href: '/docs/getting-started/quick-start', title: 'Quick start', note: 'Five minutes to your first cluster' },
  { href: '/docs/reference/commands', title: 'All commands', note: 'Every command and flag' },
  { href: '/docs/faq', title: 'Questions', note: 'Short answers to common ones' },
];

export default function NotFound() {
  return (
    <HomeLayout {...baseOptions()}>
      <main className="flex flex-1 flex-col sm:px-4 sm:py-4">
        <div className="relative isolate flex flex-1 flex-col overflow-hidden bg-[linear-gradient(180deg,#0b1b2e_0%,#0d2338_100%)] text-[#eef3f7] ring-1 ring-white/[0.06] sm:rounded-[2rem]">
          <Stars className="pointer-events-none absolute inset-0 -z-10 h-full w-full" />
          <div className="mx-auto w-full max-w-2xl px-5 pt-16 text-center sm:pt-24">
            <p className="font-mono text-sm tracking-widest text-[#ff7a47]">404</p>
            <h1 className="mt-4 font-display text-4xl font-extrabold tracking-[-0.03em] sm:text-5xl">
              This page has drifted out of the harbour.
            </h1>
            <p className="mt-4 text-[17px] leading-relaxed text-[#a9bccb]">
              The link may be old, or the page may have moved. Search the docs, or pick up from one of these.
            </p>
            <FullSearchTrigger className="mx-auto mt-8 w-full max-w-md bg-white/[0.06] text-[#c8d4df] ring-1 ring-white/10 hover:bg-white/10" />
            <ul className="mt-8 grid gap-3 text-left sm:grid-cols-2">
              {links.map((l) => (
                <li key={l.href}>
                  <Link
                    href={l.href}
                    className="group flex items-center justify-between gap-3 rounded-xl border border-white/10 bg-white/[0.04] px-4 py-3 transition hover:bg-white/[0.08]"
                  >
                    <span>
                      <span className="block font-semibold">{l.title}</span>
                      <span className="block text-sm text-[#8ea3b5]">{l.note}</span>
                    </span>
                    <ArrowRight className="size-4 shrink-0 text-[#ff7a47] transition-transform group-hover:translate-x-0.5" />
                  </Link>
                </li>
              ))}
            </ul>
          </div>
          <NightScene className="mt-auto block h-[160px] w-full pt-6 sm:h-[220px]" />
        </div>
      </main>
    </HomeLayout>
  );
}
