'use client';

import { type ReactNode, useEffect, useMemo, useRef, useState } from 'react';
import { NightScene } from './night-scene';
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
 * The hero's moving parts: a terminal typing a tug session, chapters that
 * tell its story, and the harbour scene, all driven by one timeline.
 */
export function HeroStage({ copy }: { copy: ReactNode }) {
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

  // Pause while the hero is off screen or the tab is hidden.
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
    <div ref={rootRef}>
      <div className="mx-auto grid w-full max-w-6xl grid-cols-1 gap-12 px-5 pt-14 sm:px-10 sm:pt-20 lg:grid-cols-[minmax(0,1.05fr)_minmax(0,1fr)] lg:items-center lg:gap-14 lg:pt-24">
        <div className="min-w-0">{copy}</div>

        <div className="min-w-0 animate-rise [animation-delay:250ms]">
          <div
            className="relative min-w-0 overflow-hidden rounded-2xl border border-white/10 bg-[#060e18]/90 shadow-[0_40px_90px_-30px_rgba(0,0,0,0.8)] backdrop-blur"
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
              className="h-[19rem] overflow-hidden whitespace-pre-wrap break-words px-4 py-3 font-mono text-[11.5px] leading-[1.7] text-[#dbe6ef] sm:h-[21rem] sm:text-[12.5px]"
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

          {/* The story so far: four chapters that fill as the session plays. */}
          <nav className="mt-4 grid grid-cols-4 gap-2.5" aria-label="Story chapters">
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
                  className="group min-w-0 rounded-lg pt-1 text-left outline-none focus-visible:ring-2 focus-visible:ring-[#ff7a47]/60"
                >
                  <div className="h-[3px] overflow-hidden rounded-full bg-white/10">
                    <div
                      className="h-full rounded-full bg-[#ff7a47] transition-[width] duration-300 ease-linear motion-reduce:transition-none"
                      style={{ width: `${Math.round(p * 100)}%` }}
                    />
                  </div>
                  <p
                    className={`mt-2 truncate text-[12px] font-semibold transition-colors sm:text-[13px] ${active ? 'text-white' : 'text-[#7b8fa3] group-hover:text-[#c8d4df]'}`}
                  >
                    <span className="mr-1.5 font-mono text-[10px] text-[#ff7a47]/80">0{i + 1}</span>
                    {c.title}
                  </p>
                </button>
              );
            })}
          </nav>
          <p
            key={chapter}
            className="mt-3 min-h-[3.2em] animate-line-in text-[13px] leading-relaxed text-[#8ea3b5] sm:text-[13.5px]"
            aria-live="polite"
          >
            {chapters[chapter].caption}
          </p>
        </div>
      </div>

      <NightScene scene={scene} still={reduced} className="mt-2 block h-[160px] w-full sm:h-[230px] lg:h-[270px]" />
    </div>
  );
}
