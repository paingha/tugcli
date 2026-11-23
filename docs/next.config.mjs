import { execFileSync } from 'node:child_process';
import { createMDX } from 'fumadocs-mdx/next';

const withMDX = createMDX();

// Set when building for a host:
//   DOCS_BASE_PATH  the sub-path the site is served from, e.g. /tugcli
//   DOCS_SITE_URL   the public address, including the base path, e.g.
//                   https://paingha.github.io/tugcli
//   DOCS_VERSION    the tug version the docs describe (default: main)
//   DOCS_RELEASE    the latest tug release, which the home page announces
//                   (default: the highest vX.Y.Z tag in git)
const basePath = process.env.DOCS_BASE_PATH ?? '';

// latestRelease is the highest tug release tagged in git, such as v0.3.0.
// Pre-releases and the SDKs' tags (sdk/python/v0.1.0) are left out, as
// GitHub leaves pre-releases out of its latest release. Without git or tags,
// as in a shallow clone, it is empty and the home page links to the latest
// release without naming it.
function latestRelease() {
  let out;
  try {
    out = execFileSync('git', ['tag', '--list', 'v[0-9]*'], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] });
  } catch {
    return '';
  }
  const versions = out
    .split('\n')
    .map((tag) => /^v(\d+)\.(\d+)\.(\d+)$/.exec(tag.trim()))
    .filter((m) => m !== null)
    .map((m) => ({ tag: m[0], parts: m.slice(1).map(Number) }));
  versions.sort((a, b) => b.parts[0] - a.parts[0] || b.parts[1] - a.parts[1] || b.parts[2] - a.parts[2]);
  return versions[0]?.tag ?? '';
}

/** @type {import('next').NextConfig} */
const config = {
  output: 'export',
  reactStrictMode: true,
  basePath: basePath || undefined,
  trailingSlash: true,
  // A static export has no server to resize images, so pages link to the
  // files themselves.
  images: { unoptimized: true },
  env: {
    NEXT_PUBLIC_BASE_PATH: basePath,
    NEXT_PUBLIC_SITE_URL: process.env.DOCS_SITE_URL ?? '',
    NEXT_PUBLIC_DOCS_VERSION: process.env.DOCS_VERSION ?? '',
    NEXT_PUBLIC_TUG_RELEASE: process.env.DOCS_RELEASE || latestRelease(),
  },
};

export default withMDX(config);
