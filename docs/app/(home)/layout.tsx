import Link from 'next/link';
import { HomeLayout } from 'fumadocs-ui/layouts/home';
import { baseOptions } from '@/lib/layout.shared';

export default function Layout({ children }: LayoutProps<'/'>) {
  const base = baseOptions();
  return (
    <HomeLayout
      {...base}
      nav={{ ...base.nav, transparentMode: 'top' }}
      links={[
        ...(base.links ?? []),
        {
          type: 'custom',
          secondary: true,
          children: (
            <Link
              href="/docs/getting-started/installation"
              className="ms-1.5 inline-flex items-center rounded-full bg-fd-foreground px-3.5 py-1.5 text-sm font-semibold text-fd-background transition hover:opacity-85"
            >
              Get started
            </Link>
          ),
        },
      ]}
    >
      {children}
    </HomeLayout>
  );
}
