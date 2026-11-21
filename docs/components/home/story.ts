// The hero's story: a short tug session in four chapters. The terminal types
// it out, and the harbour scene below reacts to each line as it appears.
// Output text matches what tug really prints.

export type Tone = 'plain' | 'ok' | 'err' | 'warn' | 'dim' | 'bold' | 'podA' | 'podB';
export type Segment = [string, Tone?];
export type Mode = 'none' | 'normal' | 'readonly' | 'unlocked';

/** What the scene shows at a moment in the story. */
export interface SceneState {
  loaded: number; // containers on the ship
  mode: Mode; // the session's mode, shown by the tug's flag
  focus: boolean; // connected: the terminal's title shows the cluster
  logs: boolean; // the ship's bridge flickers while logs stream
  alarm: number; // bumps when tug refuses a change
  restart: number; // bumps when a workload restarts
}

type Effect = Partial<Omit<SceneState, 'alarm' | 'restart'>> & { alarm?: true; restart?: true };

export interface Line {
  segments: Segment[];
  effect?: Effect;
}

export interface Step {
  chapter: number;
  mode: Mode;
  where?: string;
  cmd: string;
  out: Line[];
}

export const chapters = [
  {
    title: 'Find',
    caption: 'One command finds clusters in kubeconfig files, local tools and cloud accounts, and loads them into tug’s catalog.',
  },
  {
    title: 'Connect',
    caption: 'This terminal connects to prod-eu. It’s production, so the session opens read-only and the tug raises a red flag.',
  },
  {
    title: 'Work',
    caption: 'Logs from every pod of the api deployment stream side by side, each with its own coloured prefix.',
  },
  {
    title: 'Stay safe',
    caption: 'A risky delete is refused. Unlock writes for 15 minutes when you mean it, and the flag turns amber.',
  },
];

const line = (segments: Segment[], effect?: Effect): Line => ({ segments, effect });

export const script: Step[] = [
  {
    chapter: 0,
    mode: 'none',
    cmd: 'tug import --yes',
    out: [
      line([['✓ ', 'ok'], ['kubeconfig/default: 3 clusters, 3 new']], { loaded: 3 }),
      line([['✓ ', 'ok'], ['aws/work: 4 clusters, 4 new']], { loaded: 7 }),
      line([['✓ ', 'ok'], ['local/kind: 3 clusters, 3 new']], { loaded: 10 }),
    ],
  },
  {
    chapter: 1,
    mode: 'none',
    cmd: 'tug connect prod',
    out: [
      line(
        [
          ['Connected to '],
          ['prod-eu', 'bold'],
          [', namespace default, '],
          ['read-only', 'err'],
          [' (tug unlock 15m to allow writes)', 'dim'],
        ],
        { mode: 'readonly', focus: true },
      ),
    ],
  },
  {
    chapter: 1,
    mode: 'readonly',
    where: 'prod-eu:default',
    cmd: 'tug ns pay',
    out: [line([['Namespace '], ['payments', 'bold'], [' on prod-eu']])],
  },
  {
    chapter: 2,
    mode: 'readonly',
    where: 'prod-eu:payments',
    cmd: 'tug logs api -f',
    out: [
      line([['api-7d9c-xk2p/api ', 'podA'], ['GET /healthz 200']], { logs: true }),
      line([['api-7d9c-m4qz/api ', 'podB'], ['POST /orders 201']]),
      line([['api-7d9c-xk2p/api ', 'podA'], ['GET /orders/9921 200']]),
      line([['^C', 'dim']], { logs: false }),
    ],
  },
  {
    chapter: 3,
    mode: 'readonly',
    where: 'prod-eu:payments',
    cmd: 'tug delete pod debug',
    out: [
      line([['prod-eu is read-only. Run `kubectl delete pod debug` anyway? [y/N] '], ['n', 'bold']]),
      line([['tug: refused: prod-eu is read-only and `kubectl delete pod debug` may write; …', 'err']], { alarm: true }),
    ],
  },
  {
    chapter: 3,
    mode: 'readonly',
    where: 'prod-eu:payments',
    cmd: 'tug unlock 15m',
    out: [
      line([['Unlocked:', 'warn'], [' writes allowed on '], ['prod-eu', 'bold'], [' until 3:45PM']], {
        mode: 'unlocked',
      }),
    ],
  },
  {
    chapter: 3,
    mode: 'unlocked',
    where: 'prod-eu:payments',
    cmd: 'tug restart api',
    out: [line([['Restarted deploy/api']], { restart: true })],
  },
];

/** A point in the story: steps fully shown, then progress through the next. */
export interface Frame {
  step: number;
  typed: number;
  lines: number;
}

export const startFrame: Frame = { step: 0, typed: 0, lines: 0 };
export const endFrame: Frame = { step: script.length, typed: 0, lines: 0 };

export const emptyScene: SceneState = { loaded: 0, mode: 'none', focus: false, logs: false, alarm: 0, restart: 0 };

/** Replays every effect up to the frame, so any frame can be shown directly. */
export function sceneAt(frame: Frame): SceneState {
  const s = { ...emptyScene };
  const apply = (e?: Effect) => {
    if (!e) return;
    const { alarm, restart, ...rest } = e;
    Object.assign(s, rest);
    if (alarm) s.alarm++;
    if (restart) s.restart++;
  };
  script.forEach((step, i) => {
    if (i < frame.step) step.out.forEach((l) => apply(l.effect));
    else if (i === frame.step) step.out.slice(0, frame.lines).forEach((l) => apply(l.effect));
  });
  return s;
}

/** The chapter a frame belongs to. */
export function chapterAt(frame: Frame): number {
  return frame.step >= script.length ? chapters.length - 1 : script[frame.step].chapter;
}

/** How far through chapter c the frame is, from 0 to 1. */
export function chapterProgress(frame: Frame, c: number): number {
  const steps = script.map((s, i) => ({ s, i })).filter(({ s }) => s.chapter === c);
  const first = steps[0].i;
  const last = steps[steps.length - 1].i;
  if (frame.step > last) return 1;
  if (frame.step < first) return 0;
  const s = script[frame.step];
  const within = 0.5 * (frame.typed / s.cmd.length) + 0.5 * (frame.lines / s.out.length);
  return (frame.step - first + within) / steps.length;
}

/** The first frame of chapter c, or the frame just after it ends. */
export function chapterStart(c: number): Frame {
  return { step: script.findIndex((s) => s.chapter === c), typed: 0, lines: 0 };
}

export function chapterEnd(c: number): Frame {
  const last = script.reduce((acc, s, i) => (s.chapter === c ? i : acc), 0);
  return { step: last + 1, typed: 0, lines: 0 };
}
