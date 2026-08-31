import type { Metadata } from 'next';
import type { ReactNode } from 'react';
import { Provider } from '@/components/provider';
import { absoluteUrl, withBasePath } from '@/lib/site.mjs';
import './global.css';

export const metadata: Metadata = {
  metadataBase: new URL(absoluteUrl('/')),
  title: { default: 'Mango documentation', template: '%s · Mango' },
  description: 'An independent, self-hosted runtime for durable AI agents.',
  icons: { icon: withBasePath('/img/mango-mark.svg') },
};

export default function Layout({ children }: { children: ReactNode }) {
  return (
    <html lang="en" suppressHydrationWarning>
      <body className="flex min-h-screen flex-col"><Provider>{children}</Provider></body>
    </html>
  );
}
