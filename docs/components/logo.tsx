import type { SVGProps } from 'react';

/** The tug mark: a small tugboat riding a wave. */
export function LogoMark(props: SVGProps<SVGSVGElement>) {
  return (
    <svg viewBox="0 0 32 32" fill="none" aria-hidden="true" {...props}>
      <rect x="17.5" y="4" width="5" height="8" rx="1.2" fill="var(--color-tug)" />
      <rect x="17.5" y="6.2" width="5" height="1.8" fill="var(--color-fd-background)" opacity="0.9" />
      <path
        d="M9 11.5c0-.8.7-1.5 1.5-1.5h11c.8 0 1.5.7 1.5 1.5V17H9v-5.5Z"
        fill="currentColor"
      />
      <rect x="11.3" y="12.4" width="2.6" height="2.4" rx="0.6" fill="var(--color-fd-background)" />
      <rect x="15.4" y="12.4" width="2.6" height="2.4" rx="0.6" fill="var(--color-fd-background)" />
      <path
        d="M2.5 17h27l-2.6 6.6a2 2 0 0 1-1.86 1.27H7.2a2 2 0 0 1-1.8-1.13L2.5 17Z"
        fill="var(--color-tug)"
      />
      <path
        d="M2 28.4c2.2 0 2.2-1.4 4.4-1.4s2.2 1.4 4.4 1.4 2.2-1.4 4.4-1.4 2.2 1.4 4.4 1.4 2.2-1.4 4.4-1.4 2.2 1.4 4.4 1.4"
        stroke="var(--color-harbour)"
        strokeWidth="1.8"
        strokeLinecap="round"
      />
    </svg>
  );
}

export function Logo() {
  return (
    <span className="inline-flex items-center gap-2">
      <LogoMark className="size-6" />
      <span className="font-display text-[1.15rem] font-bold tracking-tight">tug</span>
    </span>
  );
}
