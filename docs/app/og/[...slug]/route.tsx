import { notFound } from 'next/navigation';
import { ogImage } from '@/lib/og';
import { source } from '@/lib/source';

export const dynamic = 'force-static';
export const dynamicParams = false;

// Preview images, each at a path ending in .png so static hosts serve the
// right content type:
//   /og/site/image.png          the landing page
//   /og/docs/<page>/image.png   a docs page
export async function GET(_req: Request, { params }: RouteContext<'/og/[...slug]'>) {
  const { slug } = await params;
  if (slug[0] === 'site') {
    return ogImage({
      title: 'Every cluster, one tug away.',
      description:
        'Find your Kubernetes clusters, keep their keys in your keychain, and give every terminal a cluster of its own.',
    });
  }
  const page = source.getPage(slug.slice(1, -1));
  if (!page) notFound();
  const section = page.slugs.length > 0 ? sectionName(page.slugs[0]) : 'Guide';
  return ogImage({ title: page.data.title, description: page.data.description, section });
}

export function generateStaticParams() {
  return [
    { slug: ['site', 'image.png'] },
    ...source.getPages().map((page) => ({
      slug: [...page.url.split('/').filter(Boolean), 'image.png'],
    })),
  ];
}

function sectionName(folder: string): string {
  const names: Record<string, string> = {
    'getting-started': 'Getting started',
    concepts: 'Concepts',
    guides: 'Guides',
    providers: 'Providers',
    plugins: 'Plugins',
    reference: 'Reference',
  };
  return names[folder] ?? 'Guide';
}
