export const appName = 'tug';
export const docsRoute = '/docs';

// Prefix for URLs that next/link does not rewrite, such as fetches.
export const basePath = process.env.NEXT_PUBLIC_BASE_PATH ?? '';

// The public address of the site, including any base path, for canonical
// URLs, the sitemap and link previews. Set DOCS_SITE_URL when building.
export const siteUrl = (process.env.NEXT_PUBLIC_SITE_URL || `http://localhost:3000${basePath}`).replace(/\/$/, '');

// Which tug the docs describe, shown in the sidebar. Set DOCS_VERSION when
// building a release's docs.
export const docsVersion = process.env.NEXT_PUBLIC_DOCS_VERSION || 'main';

export const gitConfig = {
  user: 'paingha',
  repo: 'tugcli',
  branch: 'main',
};

export const repoUrl = `https://github.com/${gitConfig.user}/${gitConfig.repo}`;

/** The preview image for a docs page, e.g. /og/docs/concepts/sessions/image.png. */
export function ogImagePath(pageUrl: string): string {
  return `/og${pageUrl}/image.png`;
}
