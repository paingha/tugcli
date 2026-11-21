import type { Metadata, Viewport } from 'next';
import { Bricolage_Grotesque, Inter, JetBrains_Mono } from 'next/font/google';
import { Provider } from '@/components/provider';
import { siteUrl } from '@/lib/shared';
import './global.css';

const heading = Bricolage_Grotesque({
  subsets: ['latin'],
  variable: '--font-heading',
  display: 'swap',
});

const body = Inter({
  subsets: ['latin'],
  variable: '--font-body',
  display: 'swap',
});

const code = JetBrains_Mono({
  subsets: ['latin'],
  variable: '--font-code',
  display: 'swap',
});

export const metadata: Metadata = {
  // Trailing slash so Next joins page paths onto any base path.
  metadataBase: new URL(`${siteUrl}/`),
  title: {
    template: '%s · tug',
    default: 'tug: every Kubernetes cluster, one command away',
  },
  description:
    'tug finds your Kubernetes clusters, keeps their credentials in your keychain, and gives each terminal its own cluster.',
  alternates: { canonical: './' },
  openGraph: {
    type: 'website',
    siteName: 'tug',
    locale: 'en_GB',
    images: [{ url: '/og/site/image.png', width: 1200, height: 630 }],
  },
  twitter: { card: 'summary_large_image', images: ['/og/site/image.png'] },
};

export const viewport: Viewport = {
  themeColor: [
    { media: '(prefers-color-scheme: light)', color: '#fbfaf7' },
    { media: '(prefers-color-scheme: dark)', color: '#070f18' },
  ],
};

export default function Layout({ children }: LayoutProps<'/'>) {
  return (
    <html
      lang="en-GB"
      className={`${heading.variable} ${body.variable} ${code.variable}`}
      suppressHydrationWarning
    >
      <body className="flex flex-col min-h-screen font-sans antialiased">
        <Provider>{children}</Provider>
      </body>
    </html>
  );
}
