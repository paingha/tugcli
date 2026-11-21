import type { BaseLayoutProps } from 'fumadocs-ui/layouts/shared';
import { Logo } from '@/components/logo';
import { repoUrl } from './shared';

export function baseOptions(): BaseLayoutProps {
  return {
    nav: {
      title: <Logo />,
    },
    githubUrl: repoUrl,
    links: [
      { text: 'Guide', url: '/docs', active: 'nested-url' },
      { text: 'Commands', url: '/docs/reference/commands', active: 'nested-url' },
      { text: 'Plugins', url: '/docs/plugins', active: 'nested-url' },
    ],
  };
}
