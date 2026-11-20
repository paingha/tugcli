// The hero's harbour at night: a tug towing a ship that fills with clusters
// as the story plays. It is sized by its container and cropped from the sides
// on narrow screens, so the convoy always stays in view.

import type { CSSProperties } from 'react';
import { emptyScene, type Mode, type SceneState } from './story';

// Every water layer slides 200 units per loop, so wave lengths must divide
// 200 for the loop to be seamless.
function crest(y: number, amp: number, period: 100 | 200 = 200, from = -200, to = 1600): string {
  let d = `M${from} ${y}`;
  for (let x = from; x < to; x += period) {
    d += ` q ${period / 4} ${-amp} ${period / 2} 0 q ${period / 4} ${amp} ${period / 2} 0`;
  }
  return d;
}

function wave(y: number, amp: number, period: 100 | 200 = 200): string {
  return `${crest(y, amp, period)} V 300 H -200 Z`;
}

// The water flows right, as the convoy sails left; nearer water moves faster.
const drift = (still: boolean | undefined, seconds: number): CSSProperties | undefined =>
  still ? undefined : { animation: `sea-shift ${seconds}s linear infinite reverse` };

// A crest of the waterline swell passes every SWELL seconds; boats pitch
// and lift on the same beat.
const SWELL = 6;

/** Pitch about the waterline and lift, timed to the swell. */
function Swell({ cx, cy, pitch, lift, delay }: { cx: number; cy: number; pitch: number; lift: number; delay: number }) {
  const ease = { calcMode: 'spline', keyTimes: '0;0.5;1', keySplines: '0.45 0 0.55 1;0.45 0 0.55 1' } as const;
  return (
    <>
      <animateTransform
        attributeName="transform"
        type="rotate"
        values={`${-pitch} ${cx} ${cy};${pitch} ${cx} ${cy};${-pitch} ${cx} ${cy}`}
        dur={`${SWELL}s`}
        begin={`${-delay}s`}
        repeatCount="indefinite"
        {...ease}
      />
      <animateTransform
        attributeName="transform"
        type="translate"
        additive="sum"
        values={`0 0;0 ${-lift};0 0`}
        dur={`${SWELL}s`}
        begin={`${-(delay + SWELL / 4)}s`}
        repeatCount="indefinite"
        {...ease}
      />
    </>
  );
}

const sourceColour = { kube: '#4f72c4', aws: '#c4734a', local: '#3d9468' } as const;

// In loading order: kubeconfig's three, then AWS's four, then kind's three.
const containers: { x: number; y: number; label: string; src: keyof typeof sourceColour }[] = [
  { x: 296, y: 70, label: 'prod-eu', src: 'kube' },
  { x: 342, y: 70, label: 'staging', src: 'kube' },
  { x: 388, y: 70, label: 'dev-tools', src: 'kube' },
  { x: 434, y: 70, label: 'eks-api', src: 'aws' },
  { x: 480, y: 70, label: 'eks-batch', src: 'aws' },
  { x: 526, y: 70, label: 'prod-us', src: 'aws' },
  { x: 342, y: 49, label: 'eks-ml', src: 'aws' },
  { x: 388, y: 49, label: 'kind-dev', src: 'local' },
  { x: 434, y: 49, label: 'kind-ci', src: 'local' },
  { x: 480, y: 49, label: 'kind-e2e', src: 'local' },
];
const batchStart = (i: number) => (i < 3 ? 0 : i < 7 ? 3 : 7);

const modeColour: Record<Mode, string> = {
  none: '#5fd4dc',
  normal: '#5fd4dc',
  readonly: '#ff5c6c',
  unlocked: '#ffcf5c',
};

const anim = (still: boolean | undefined, animation: string, extra?: CSSProperties): CSSProperties =>
  still ? { ...extra } : { transformBox: 'fill-box', transformOrigin: 'center', animation, ...extra };

function Tug({ scene, still }: { scene: SceneState; still?: boolean }) {
  const flag = modeColour[scene.mode];
  return (
    <g>
      {!still && <Swell cx={98} cy={118} pitch={1.1} lift={1.6} delay={0} />}
      {/* Funnel smoke: a steady trickle, and a big puff on each restart. */}
      {!still &&
        [0, 1, 2].map((i) => (
          <circle
            key={i}
            cx={143}
            cy={38}
            r={5 + i * 2}
            fill="#c8d4df"
            opacity={0}
            style={anim(still, `smoke 4.5s ease-out ${i * 1.5}s infinite`)}
          />
        ))}
      {scene.restart > 0 && !still && (
        <g key={scene.restart}>
          {[0, 1, 2, 3].map((i) => (
            <circle
              key={i}
              cx={140 + i * 3}
              cy={36}
              r={7 + i * 2.5}
              fill="#e6edf3"
              opacity={0}
              style={anim(still, `burst 1.8s ease-out ${i * 0.12}s 1 both`)}
            />
          ))}
        </g>
      )}

      {/* Tow line, hanging slack, tucked under the ship's bow. */}
      <path
        d="M168 97 Q216 124 266 96"
        fill="none"
        stroke="#8ea3b5"
        strokeWidth="1.6"
        strokeDasharray="1 4.5"
        strokeLinecap="round"
      />

      {/* Mast and the session flag: teal, red for read-only, amber unlocked. */}
      <line x1="98" y1="54" x2="98" y2="28" stroke="#c8d4df" strokeWidth="2" />
      <path d="M98 28 l14 4 l-14 4 z" fill={flag} style={{ transition: 'fill 0.6s ease' }}>
        {!still && (
          <animate
            attributeName="d"
            dur="1.8s"
            repeatCount="indefinite"
            values="M98 28 l14 4 l-14 4 z;M98 28 l14 2.5 l-14 5.5 z;M98 28 l14 4 l-14 4 z"
          />
        )}
      </path>
      <circle cx="98" cy="27" r="1.6" fill="#c8d4df" />

      <rect x="133" y="44" width="18" height="34" rx="3" fill="#ff7a47" />
      <rect x="133" y="50" width="18" height="5" fill="#0b1b2b" />
      <rect x="80" y="54" width="40" height="20" rx="3" fill="#e9e2d4" />
      {[85, 96, 107].map((x) => (
        <rect key={x} x={x} y="59" width="8" height="7" rx="1.5" fill="#ffd36b" />
      ))}
      <rect x="66" y="72" width="80" height="26" rx="4" fill="#e9e2d4" />
      {[82, 98, 114].map((x) => (
        <circle key={x} cx={x} cy="85" r="4" fill="#ffd36b" />
      ))}
      <path d="M24 96 H172 L163 121 a6 6 0 0 1 -5.6 4 H52 a8 8 0 0 1 -7 -4.2 Z" fill="#ff7a47" />
      <path d="M30 103 H169" stroke="#0b1b2b" strokeWidth="3" opacity="0.7" />
      <circle cx="40" cy="110" r="5" stroke="#0b1b2b" strokeWidth="2.5" opacity="0.8" />
    </g>
  );
}

function Ship({ scene, still }: { scene: SceneState; still?: boolean }) {
  return (
    <g>
      {/* The crest reaches the ship after the tug, so it moves a beat later. */}
      {!still && <Swell cx={444} cy={118} pitch={0.45} lift={1.1} delay={1.4} />}
      <rect x="572" y="40" width="44" height="52" rx="3" fill="#e9e2d4" />
      <rect
        x="578"
        y="48"
        width="32"
        height="7"
        rx="1.5"
        fill="#ffd36b"
        style={scene.logs && !still ? { animation: 'flicker 0.9s steps(2) infinite' } : undefined}
      />
      <rect
        x="578"
        y="60"
        width="32"
        height="5"
        rx="1.5"
        fill="#ffd36b"
        opacity="0.55"
        style={scene.logs && !still ? { animation: 'flicker 1.3s steps(2) 0.3s infinite' } : undefined}
      />
      <rect x="596" y="22" width="14" height="20" rx="2" fill="#ff7a47" />
      <line x1="586" y1="40" x2="586" y2="26" stroke="#c8d4df" strokeWidth="1.5" />
      <circle
        cx="586"
        cy="25"
        r="2"
        fill="#ff5c6c"
        style={still ? undefined : { animation: 'blink 2.4s ease-in-out infinite' }}
      />

      {containers.map((c, i) => {
        const on = i < scene.loaded;
        const delay = (i - batchStart(i)) * 0.12;
        const body = (
          <g
            key={c.label}
            style={{
              transform: on ? 'translateY(0)' : 'translateY(-34px)',
              opacity: on ? 1 : 0,
              transition: still
                ? 'none'
                : `transform 0.7s cubic-bezier(.34,1.56,.64,1) ${delay}s, opacity 0.45s ease ${delay}s`,
            }}
          >
            <rect x={c.x} y={c.y} width="44" height="20" rx="2" fill={sourceColour[c.src]} />
            <path
              d={`M${c.x + 4} ${c.y + 3} v14 M${c.x + 40} ${c.y + 3} v14`}
              stroke="#fff"
              strokeOpacity="0.18"
              strokeWidth="1.2"
            />
            <text
              x={c.x + 22}
              y={c.y + 13.5}
              textAnchor="middle"
              fontSize="7.5"
              fontFamily="var(--font-code), ui-monospace, monospace"
              fontWeight="600"
              fill="#fff"
            >
              {c.label}
            </text>
          </g>
        );
        if (i !== 0) return body;
        // prod-eu: shakes when tug refuses a change.
        return (
          <g
            key={`prod-${scene.alarm}`}
            style={scene.alarm > 0 && !still ? { animation: 'shake 0.5s ease-in-out 1' } : undefined}
          >
            {body}
          </g>
        );
      })}

      <path d="M258 90 H630 L624 122 a6 6 0 0 1 -6 5 H290 a10 10 0 0 1 -9 -5.6 Z" fill="#1d3a55" />
      <path d="M268 116 H627 L625 122 a6 6 0 0 1 -6 5 H290 a10 10 0 0 1 -9 -5.6 Z" fill="#b9404b" />
      <text x="300" y="108" fontSize="9" fontFamily="var(--font-heading), sans-serif" fontWeight="700" letterSpacing="2" fill="#c8d4df">
        YOUR CLUSTERS
      </text>
    </g>
  );
}

function Gull({ x, y, delay, still }: { x: number; y: number; delay: number; still?: boolean }) {
  return (
    <path d={`M${x} ${y} q 5 -5 10 0 q 5 -5 10 0`} stroke="#c8d4df" strokeWidth="1.5" fill="none" strokeLinecap="round">
      {!still && (
        <animate
          attributeName="d"
          dur="0.9s"
          begin={`${delay}s`}
          repeatCount="indefinite"
          values={`M${x} ${y} q 5 -5 10 0 q 5 -5 10 0;M${x} ${y} q 5 1 10 0 q 5 1 10 0;M${x} ${y} q 5 -5 10 0 q 5 -5 10 0`}
        />
      )}
    </path>
  );
}

export function NightScene({
  scene = emptyScene,
  still,
  className,
}: {
  scene?: SceneState;
  still?: boolean;
  className?: string;
}) {
  return (
    <svg
      viewBox="0 0 1200 260"
      preserveAspectRatio="xMidYMax slice"
      role="img"
      aria-label="At night, a tugboat tows a ship that fills with cluster containers, past a lighthouse"
      className={className}
    >
      <defs>
        <radialGradient id="moon-glow">
          <stop offset="0%" stopColor="#f4efe6" stopOpacity="0.35" />
          <stop offset="100%" stopColor="#f4efe6" stopOpacity="0" />
        </radialGradient>
        <linearGradient id="beam" x1="1" x2="0" y1="0" y2="0">
          <stop offset="0%" stopColor="#ffd36b" stopOpacity="0.4" />
          <stop offset="100%" stopColor="#ffd36b" stopOpacity="0" />
        </linearGradient>
      </defs>

      {/* Moon, with a slowly breathing halo. */}
      <circle cx="860" cy="62" r="74" fill="url(#moon-glow)" style={anim(still, 'halo 8s ease-in-out infinite')} />
      <circle cx="860" cy="62" r="17" fill="#f4efe6" />
      <circle cx="866" cy="57" r="15" fill="#0d1d30" opacity="0.18" />

      {/* Gulls crossing the sky now and then. */}
      {!still && (
        <g style={{ animation: 'gulls 46s linear infinite' }}>
          <Gull x={0} y={70} delay={0} still={still} />
          <Gull x={28} y={58} delay={0.3} still={still} />
          <Gull x={50} y={76} delay={0.55} still={still} />
        </g>
      )}

      {/* Lighthouse on its rock, sweeping a beam across the water. */}
      <path
        d="M1092 118 L560 70 L560 170 Z"
        fill="url(#beam)"
        className="max-sm:hidden"
        style={
          still
            ? undefined
            : { transformBox: 'view-box', transformOrigin: '1092px 118px', animation: 'beam 7s ease-in-out infinite alternate' }
        }
      />
      <g className="max-sm:hidden">
        <path d="M1040 222 Q1060 196 1092 194 Q1128 194 1150 222 Z" fill="#132b42" />
        <path d="M1080 196 L1084 128 H1100 L1104 196 Z" fill="#e9e2d4" />
        <path d="M1082.6 160 H1101.4 L1102.4 176 H1081.6 Z" fill="#c9505a" />
        <path d="M1083.6 138 H1100.4 L1100.9 148 H1083.1 Z" fill="#c9505a" />
        <rect x="1083" y="112" width="18" height="16" rx="2" fill="#ffd36b" />
        <path d="M1080 112 L1092 100 L1104 112 Z" fill="#c9505a" />
      </g>

      {/* Far water, behind the boats. */}
      <g style={drift(still, 30)}>
        <path d={wave(193, 5)} fill="#143552" />
        <path d={crest(193, 5)} fill="none" stroke="#2f5f88" strokeOpacity="0.45" strokeWidth="1.2" />
      </g>

      {/* The convoy, riding the swell. Hulls reach below the waterline. */}
      <g transform="translate(276 84)">
        <Tug scene={scene} still={still} />
        <Ship scene={scene} still={still} />
      </g>

      {/* Water at the waterline, in front of the hulls: the same water as
          behind them, so the boats sit in the sea rather than on it. */}
      <g style={drift(still, SWELL)}>
        <path d={wave(203, 4)} fill="#143552" />
        <path d={crest(203, 4)} fill="none" stroke="#3a6d97" strokeOpacity="0.5" strokeWidth="1.2" />
      </g>

      {/* Foam where the hulls cut the water, and the wakes behind them. */}
      <g stroke="#e6edf3" strokeLinecap="round" fill="none">
        <g style={still ? { opacity: 0.45 } : { animation: 'foam 1.8s ease-in-out infinite' }}>
          <path d="M318 201.5 q -6 -1 -11 -6 M320 204.5 q -9 0 -15 -3" strokeWidth="1.6" />
          <path d="M553 201.5 q -7 -1 -12 -7 M555 204.5 q -10 0 -17 -3" strokeWidth="1.6" />
        </g>
        <g strokeWidth="1.3" strokeDasharray="12 6" opacity="0.32" style={still ? undefined : { animation: 'wake 1.1s linear infinite' }}>
          <path d="M321 203.5 H438" />
          <path d="M557 203.5 H900" />
        </g>
        <g strokeWidth="1.3" strokeDasharray="7 11" opacity="0.28" style={still ? undefined : { animation: 'wake 1.1s linear infinite' }}>
          <path d="M440 205 q 40 3 104 2" />
          <path d="M902 205 q 60 4 160 3" />
          <path d="M900 209 q 70 8 190 7" />
        </g>
      </g>

      {/* Nearer water, moving faster. */}
      <g style={drift(still, 4.4)}>
        <path d={wave(221, 5)} fill="#0f2a42" />
      </g>

      {/* Moonlight and lit windows reflected on the water. */}
      <g style={still ? { opacity: 0.3 } : { animation: 'shimmer 3.5s ease-in-out infinite alternate' }}>
        <path d="M842 232 h36 M850 241 h22 M836 249 h46" stroke="#f4efe6" strokeWidth="2" strokeLinecap="round" />
        <path d="M360 231 h24 M366 239 h12 M854 230 h22 M860 238 h10" stroke="#ffd36b" strokeWidth="1.6" strokeLinecap="round" />
      </g>

      <g style={drift(still, 3.2)}>
        <path d={wave(240, 4, 100)} fill="#0a1b2c" />
      </g>
    </svg>
  );
}

// Deterministic stars, so the server and browser draw the same sky.
const stars = (() => {
  let seed = 7;
  const rand = () => {
    seed = (seed * 16807) % 2147483647;
    return (seed - 1) / 2147483646;
  };
  return Array.from({ length: 70 }, (_, i) => ({
    x: Math.round(rand() * 1200),
    y: Math.round(rand() * 560),
    r: rand() < 0.15 ? 1.4 : 0.8,
    twinkle: i % 6 === 0,
    delay: (rand() * 4).toFixed(2),
  }));
})();

/** A faint field of stars, with the odd shooting star, behind the hero. */
export function Stars({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 1200 560" preserveAspectRatio="xMidYMid slice" aria-hidden="true" className={className}>
      {stars.map((s, i) => (
        <circle
          key={i}
          cx={s.x}
          cy={s.y}
          r={s.r}
          fill="#dbe6ef"
          opacity={s.twinkle ? 0.7 : 0.35}
          className={s.twinkle ? 'motion-safe-only' : undefined}
          style={s.twinkle ? { animation: `twinkle 3.2s ease-in-out ${s.delay}s infinite` } : undefined}
        />
      ))}
      <g className="motion-safe-only" style={{ animation: 'shoot 11s ease-in 3s infinite', opacity: 0 }}>
        <path d="M760 60 l90 34" stroke="url(#shoot-tail)" strokeWidth="1.6" strokeLinecap="round" />
        <circle cx="850" cy="94" r="1.6" fill="#fff" />
      </g>
      <defs>
        <linearGradient id="shoot-tail" x1="0" x2="1" y1="0" y2="0">
          <stop offset="0%" stopColor="#fff" stopOpacity="0" />
          <stop offset="100%" stopColor="#fff" stopOpacity="0.9" />
        </linearGradient>
      </defs>
    </svg>
  );
}
