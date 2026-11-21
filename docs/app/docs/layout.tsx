import { source } from '@/lib/source';
import { DocsLayout } from 'fumadocs-ui/layouts/docs';
import { baseOptions } from '@/lib/layout.shared';
import { docsVersion } from '@/lib/shared';

function VersionBadge() {
  const label = /^v?\d/.test(docsVersion) ? `tug ${docsVersion.replace(/^v?/, 'v')}` : `tug ${docsVersion}`;
  return (
    <p className="flex items-center gap-2 rounded-lg border bg-fd-card px-2.5 py-1.5 text-xs text-fd-muted-foreground">
      <span className="size-1.5 rounded-full bg-(--color-harbour)" aria-hidden="true" />
      Docs for <span className="font-mono font-medium text-fd-foreground">{label}</span>
    </p>
  );
}

export default function Layout({ children }: LayoutProps<'/docs'>) {
  const { links: _links, ...options } = baseOptions();
  return (
    <DocsLayout tree={source.getPageTree()} sidebar={{ banner: <VersionBadge /> }} {...options}>
      {children}
    </DocsLayout>
  );
}
