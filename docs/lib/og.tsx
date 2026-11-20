// Link-preview images, drawn at build time with next/og. They use the site's
// typefaces from @fontsource (next/og reads WOFF, not WOFF2).

import { readFile } from 'node:fs/promises';
import { join } from 'node:path';
import { ImageResponse } from 'next/og';

export const ogSize = { width: 1200, height: 630 };

const font = (pkg: string, file: string) => readFile(join(process.cwd(), 'node_modules', '@fontsource', pkg, 'files', file));

const logo = `data:image/svg+xml;utf8,${encodeURIComponent(
  `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" fill="none"><rect x="17.5" y="4" width="5" height="8" rx="1.2" fill="#ff7a47"/><rect x="17.5" y="6.2" width="5" height="1.8" fill="#0b1b2e"/><path d="M9 11.5c0-.8.7-1.5 1.5-1.5h11c.8 0 1.5.7 1.5 1.5V17H9v-5.5Z" fill="#eef3f7"/><rect x="11.3" y="12.4" width="2.6" height="2.4" rx=".6" fill="#ffd36b"/><rect x="15.4" y="12.4" width="2.6" height="2.4" rx=".6" fill="#ffd36b"/><path d="M2.5 17h27l-2.6 6.6a2 2 0 0 1-1.86 1.27H7.2a2 2 0 0 1-1.8-1.13L2.5 17Z" fill="#ff7a47"/><path d="M2 28.4c2.2 0 2.2-1.4 4.4-1.4s2.2 1.4 4.4 1.4 2.2-1.4 4.4-1.4 2.2 1.4 4.4 1.4 2.2-1.4 4.4-1.4 2.2 1.4 4.4 1.4" stroke="#5fd4dc" stroke-width="1.8" stroke-linecap="round"/></svg>`,
)}`;

function wavePath(y: number, amp: number, period: number): string {
  let d = `M0 ${y}`;
  for (let x = 0; x < 1200; x += period) {
    d += ` q ${period / 4} ${-amp} ${period / 2} 0 q ${period / 4} ${amp} ${period / 2} 0`;
  }
  return `${d} V 140 H 0 Z`;
}

const waves = `data:image/svg+xml;utf8,${encodeURIComponent(
  `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1200 140" preserveAspectRatio="none"><path d="${wavePath(40, 14, 200)}" fill="#143552"/><path d="${wavePath(78, 10, 160)}" fill="#0f2a42"/><path d="${wavePath(108, 8, 120)}" fill="#0a1b2c"/></svg>`,
)}`;

// A few fixed stars, so every image matches.
const stars = [
  [96, 70, 3],
  [210, 150, 2],
  [380, 46, 2],
  [520, 120, 3],
  [700, 60, 2],
  [860, 140, 2],
  [990, 54, 3],
  [1110, 120, 2],
  [1040, 230, 2],
  [640, 210, 2],
] as const;

/** One preview card: the tug mark, a section label, a title and a line of text. */
export async function ogImage({
  title,
  description,
  section,
}: {
  title: string;
  description?: string;
  section?: string;
}): Promise<ImageResponse> {
  const [display, body] = await Promise.all([
    font('bricolage-grotesque', 'bricolage-grotesque-latin-800-normal.woff'),
    font('inter', 'inter-latin-400-normal.woff'),
  ]);
  const titleSize = title.length > 34 ? 64 : 80;

  return new ImageResponse(
    (
      <div
        style={{
          width: '100%',
          height: '100%',
          display: 'flex',
          flexDirection: 'column',
          position: 'relative',
          padding: '64px 76px',
          background: 'linear-gradient(180deg, #0b1b2e 0%, #0c2034 60%, #0d2338 100%)',
          color: '#eef3f7',
          fontFamily: 'Inter',
        }}
      >
        {stars.map(([x, y, r], i) => (
          <div
            key={i}
            style={{
              position: 'absolute',
              left: x,
              top: y,
              width: r * 2,
              height: r * 2,
              borderRadius: r,
              background: '#dbe6ef',
              opacity: 0.5,
            }}
          />
        ))}
        <div
          style={{
            position: 'absolute',
            right: 120,
            top: 70,
            width: 56,
            height: 56,
            borderRadius: 28,
            background: '#f4efe6',
            boxShadow: '0 0 80px 30px rgba(244,239,230,0.18)',
          }}
        />
        <div style={{ display: 'flex', alignItems: 'center' }}>
          <img src={logo} width={64} height={64} alt="" />
          <span style={{ fontFamily: 'Bricolage', fontSize: 44, marginLeft: 16, letterSpacing: '-1px' }}>tug</span>
          {section ? (
            <span
              style={{
                marginLeft: 24,
                fontSize: 22,
                color: '#8fdde3',
                border: '1px solid rgba(143,221,227,0.35)',
                borderRadius: 999,
                padding: '6px 18px',
              }}
            >
              {section}
            </span>
          ) : null}
        </div>
        <div style={{ display: 'flex', flexDirection: 'column', marginTop: 64, maxWidth: 980 }}>
          <div
            style={{
              fontFamily: 'Bricolage',
              fontSize: titleSize,
              lineHeight: 1.04,
              letterSpacing: '-2px',
            }}
          >
            {title}
          </div>
          {description ? (
            <div style={{ fontSize: 30, lineHeight: 1.4, color: '#a9bccb', marginTop: 26, maxWidth: 900 }}>
              {description}
            </div>
          ) : null}
        </div>
        <img
          src={waves}
          width={1200}
          height={140}
          alt=""
          style={{ position: 'absolute', left: 0, bottom: 0, width: 1200, height: 140 }}
        />
      </div>
    ),
    {
      ...ogSize,
      fonts: [
        { name: 'Bricolage', data: display, weight: 800, style: 'normal' },
        { name: 'Inter', data: body, weight: 400, style: 'normal' },
      ],
    },
  );
}
