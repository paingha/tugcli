import { createMDX } from 'fumadocs-mdx/next';

const withMDX = createMDX();

// Set when building for a host:
//   DOCS_BASE_PATH  the sub-path the site is served from, e.g. /tugcli
//   DOCS_SITE_URL   the public address, including the base path, e.g.
//                   https://paingha.github.io/tugcli
//   DOCS_VERSION    the tug version the docs describe (default: main)
const basePath = process.env.DOCS_BASE_PATH ?? '';

/** @type {import('next').NextConfig} */
const config = {
  output: 'export',
  reactStrictMode: true,
  basePath: basePath || undefined,
  trailingSlash: true,
  env: {
    NEXT_PUBLIC_BASE_PATH: basePath,
    NEXT_PUBLIC_SITE_URL: process.env.DOCS_SITE_URL ?? '',
    NEXT_PUBLIC_DOCS_VERSION: process.env.DOCS_VERSION ?? '',
  },
};

export default withMDX(config);
