'use client';

import { useEffect, useMemo, useRef, useState } from 'react';
import { NightScene, Stars } from './night-scene';
import {
  type Frame,
  type Mode,
  type Segment,
  type Tone,
  chapterAt,
  chapterEnd,
  chapterProgress,
  chapterStart,
  chapters,
  endFrame,
  sceneAt,
  script,
  startFrame,
} from './story';

const toneClass: Record<Tone, string> = {
  plain: '',
  ok: 'text-[#5ee3a1]',
  err: 'text-[#ff7b86]',
  warn: 'text-[#ffcf5c]',
  dim: 'text-[#7b8fa3]',
  bold: 'font-bold text-white',
  podA: 'text-[#79c0ff]',
  podB: 'text-[#d2a8ff]',
};

function PromptLabel({ mode, where }: { mode: Mode; where?: string }) {
  if (mode === 'none' || !where) return null;
  if (mode === 'normal') return <span className="text-[#5fd4dc]">⎈ {where} </span>;
  if (mode === 'readonly') return <span className="font-bold text-[#ff7b86]">⎈ {where} (read-only) </span>;
  return <span className="font-bold text-[#ffcf5c]">⎈ {where} (unlocked) </span>;
}

function Output({ segments, fresh }: { segments: Segment[]; fresh?: boolean }) {
  return (
    <div className={fresh ? 'animate-line-in' : undefined}>
      {segments.map(([text, tone = 'plain'], i) => (
        <span key={i} className={toneClass[tone]}>
          {text}
        </span>
      ))}
    </div>
  );
}

function Caret() {
  return <span className="inline-block h-[1.1em] w-[0.55em] -mb-[0.2em] bg-[#ff7a47] animate-caret" />;
}

/** The next frame and how long to wait before showing it. */
function next(frame: Frame): [Frame, number] {
  if (frame.step >= script.length) return [startFrame, 4600];
  const s = script[frame.step];
  if (frame.typed < s.cmd.length) {
    return [{ ...frame, typed: frame.typed + 1 }, frame.typed === 0 ? 700 : 42 + Math.random() * 55];
  }
  if (frame.lines < s.out.length) {
    const pause = frame.lines === 0 ? 420 : s.cmd.includes('logs') ? 560 : 260;
    return [{ ...frame, lines: frame.lines + 1 }, pause];
  }
  return [{ step: frame.step + 1, typed: 0, lines: 0 }, s.chapter !== script[frame.step + 1]?.chapter ? 1500 : 950];
}

/**
 * The hero's picture: a terminal typing a tug session over a harbour at
 * dusk, with a dock of chapters that tell its story. One timeline drives the
 * terminal, the chapters and the boats.
 */
export function HeroStage() {
  const [frame, setFrame] = useState<Frame>(startFrame);
  const [reduced, setReduced] = useState(false);
  const [hovered, setHovered] = useState(false);
  const [visible, setVisible] = useState(true);
  const rootRef = useRef<HTMLDivElement>(null);
  const bodyRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const mq = window.matchMedia('(prefers-reduced-motion: reduce)');
    const update = () => {
      setReduced(mq.matches);
      if (mq.matches) setFrame(endFrame);
    };
    update();
    mq.addEventListener('change', update);
    return () => mq.removeEventListener('change', update);
  }, []);

  // Pause while the picture is off screen or the tab is hidden.
  useEffect(() => {
    const el = rootRef.current;
    if (!el) return;
    const io = new IntersectionObserver(([e]) => setVisible(e.isIntersecting), { threshold: 0.05 });
    io.observe(el);
    const onVis = () => setVisible(document.visibilityState === 'visible');
    document.addEventListener('visibilitychange', onVis);
    return () => {
      io.disconnect();
      document.removeEventListener('visibilitychange', onVis);
    };
  }, []);

  const paused = reduced || hovered || !visible;

  useEffect(() => {
    if (paused) return;
    const [n, delay] = next(frame);
    const id = window.setTimeout(() => setFrame(n), delay);
    return () => window.clearTimeout(id);
  }, [frame, paused]);

  useEffect(() => {
    const el = bodyRef.current;
    if (el) el.scrollTo({ top: el.scrollHeight, behavior: reduced ? 'auto' : 'smooth' });
  }, [frame, reduced]);

  const scene = useMemo(() => sceneAt(frame), [frame]);
  const chapter = chapterAt(frame);
  const done = script.slice(0, frame.step);
  const current = frame.step < script.length ? script[frame.step] : null;
  const last = script[script.length - 1];
  const title = scene.focus ? '⎈ prod-eu' : '~/src';

  const jump = (c: number) => setFrame(reduced ? chapterEnd(c) : chapterStart(c));

  return (
    <div
      ref={rootRef}
      className="relative isolate overflow-hidden rounded-[22px] bg-[linear-gradient(180deg,#0e1834_0%,#1f2a58_34%,#4b3f78_60%,#a3607e_80%,#ec9a72_93%,#f6b67f_100%)] shadow-[0_30px_80px_-40px_rgba(30,20,70,0.55)] ring-1 ring-black/5 sm:rounded-[32px] dark:ring-white/10"
    >
      {/* A painted dusk: stars overhead, soft clouds, and the low sun's glow. */}
      <Stars className="pointer-events-none absolute inset-x-0 top-0 -z-10 h-[60%] w-full opacity-70" />
      <div
        aria-hidden="true"
        className="pointer-events-none absolute inset-x-[-10%] bottom-[8%] -z-10 h-[45%] bg-[radial-gradient(50%_60%_at_50%_100%,rgba(255,186,120,0.55),transparent_70%)]"
      />
      <div aria-hidden="true" className="pointer-events-none absolute inset-0 -z-10 motion-safe:animate-[drift-cloud_26s_ease-in-out_infinite_alternate]">
        <div className="absolute left-[-6%] top-[38%] h-16 w-[38%] rounded-full bg-[#f3c6c9]/20 blur-2xl" />
        <div className="absolute right-[-4%] top-[30%] h-20 w-[34%] rounded-full bg-[#c9b8ff]/15 blur-2xl" />
        <div className="absolute left-[30%] top-[62%] h-14 w-[40%] rounded-full bg-[#ffd2b0]/20 blur-2xl" />
      </div>

      <div className="relative mx-auto w-full max-w-[54rem] px-3 pt-5 sm:px-8 sm:pt-12">
        <div
          className="relative min-w-0 overflow-hidden rounded-xl border border-white/10 bg-[#070f1c]/92 shadow-[0_40px_100px_-30px_rgba(5,6,30,0.85)] backdrop-blur sm:rounded-2xl"
          onMouseEnter={() => setHovered(true)}
          onMouseLeave={() => setHovered(false)}
        >
          <div className="flex items-center gap-2 border-b border-white/[0.07] px-4 py-3">
            <span className="size-2.5 rounded-full bg-[#ff5f57]" />
            <span className="size-2.5 rounded-full bg-[#febc2e]" />
            <span className="size-2.5 rounded-full bg-[#28c840]" />
            <span key={title} className="ml-2 animate-line-in font-mono text-[11px] text-[#7b8fa3]">
              zsh: {title}
            </span>
            <span
              className={`ml-auto font-mono text-[10px] uppercase tracking-wider text-[#7b8fa3] transition-opacity duration-300 ${hovered && !reduced ? 'opacity-100' : 'opacity-0'}`}
            >
              paused
            </span>
          </div>
          <div
            ref={bodyRef}
            aria-label="An example tug session: importing clusters, connecting to production read-only, following logs, and unlocking writes"
            role="img"
            className="h-[17rem] overflow-hidden whitespace-pre-wrap break-words px-4 py-3 font-mono text-[11px] leading-[1.7] text-[#dbe6ef] sm:h-[20rem] sm:text-[12.5px]"
          >
            {done.map((s, i) => (
              <div key={i}>
                <div>
                  <PromptLabel mode={s.mode} where={s.where} />
                  <span className="text-[#7b8fa3]">$ </span>
                  {s.cmd}
                </div>
                {s.out.map((l, j) => (
                  <Output key={j} segments={l.segments} />
                ))}
              </div>
            ))}
            {current ? (
              <div>
                <div>
                  <PromptLabel mode={current.mode} where={current.where} />
                  <span className="text-[#7b8fa3]">$ </span>
                  {current.cmd.slice(0, frame.typed)}
                  {frame.typed < current.cmd.length || frame.lines === 0 ? <Caret /> : null}
                </div>
                {current.out.slice(0, frame.lines).map((l, j) => (
                  <Output key={j} segments={l.segments} fresh />
                ))}
              </div>
            ) : (
              <div>
                <PromptLabel mode={last.mode} where={last.where} />
                <span className="text-[#7b8fa3]">$ </span>
                <Caret />
              </div>
            )}
          </div>
        </div>

        {/* The dock: four chapters that fill as the session plays. */}
        <nav
          aria-label="Story chapters"
          className="mx-auto mt-4 grid w-full max-w-xl grid-cols-4 gap-1 rounded-2xl bg-white/10 p-1.5 ring-1 ring-white/15 backdrop-blur-md sm:mt-5"
        >
          {chapters.map((c, i) => {
            const p = reduced ? (i <= chapter ? 1 : 0) : chapterProgress(frame, i);
            const active = i === chapter;
            return (
              <button
                key={c.title}
                type="button"
                aria-current={active ? 'step' : undefined}
                aria-label={`Chapter ${i + 1}: ${c.title}`}
                onClick={() => jump(i)}
                className={`min-w-0 rounded-xl px-2 py-2 text-left outline-none transition-colors focus-visible:ring-2 focus-visible:ring-white/60 sm:px-3 ${active ? 'bg-white/15' : 'hover:bg-white/[0.07]'}`}
              >
                <p className={`truncate text-[12px] font-semibold sm:text-[13px] ${active ? 'text-white' : 'text-white/60'}`}>
                  <span className="mr-1.5 font-mono text-[10px] text-[#ffb38a] max-sm:hidden">0{i + 1}</span>
                  {c.title}
                </p>
                <div className="mt-1.5 h-[3px] overflow-hidden rounded-full bg-white/15">
                  <div
                    className="h-full rounded-full bg-[#ffb38a] transition-[width] duration-300 ease-linear motion-reduce:transition-none"
                    style={{ width: `${Math.round(p * 100)}%` }}
                  />
                </div>
              </button>
            );
          })}
        </nav>
        <p
          key={chapter}
          className="mx-auto mt-3 min-h-[3.2em] max-w-xl animate-line-in text-center text-[12.5px] leading-relaxed text-white/75 sm:text-[13.5px]"
          aria-live="polite"
        >
          {chapters[chapter].caption}
        </p>
      </div>

      <NightScene scene={scene} still={reduced} className="relative -z-[1] -mt-6 block h-[130px] w-full sm:-mt-14 sm:h-[210px] lg:h-[250px]" />
    </div>
  );
}
