import { notFound } from 'next/navigation';
import type { Metadata } from 'next';
import { DocsBody, DocsDescription, DocsPage, DocsTitle, MarkdownCopyButton } from 'fumadocs-ui/layouts/docs/page';
import { createRelativeLink } from 'fumadocs-ui/mdx';
import { source, markdownPath } from '@/lib/source';
import { getMDXComponents } from '@/components/mdx';
import { absoluteUrl, githubUrl, withBasePath } from '@/lib/site.mjs';

export const dynamicParams = false;

export default async function Page(props: PageProps<'/[[...slug]]'>) {
  const { slug } = await props.params;
  const page = source.getPage(slug);
  if (!page) notFound();
  const Content = page.data.body;
  return (
    <DocsPage toc={page.data.toc} full={page.data.full}>
      <DocsTitle>{page.data.title}</DocsTitle>
      {page.data.description && <DocsDescription className="mb-0">{page.data.description}</DocsDescription>}
      <div className="flex items-center gap-3 border-b pb-6">
        <MarkdownCopyButton markdownUrl={withBasePath(markdownPath(page))} />
        <a href={`${githubUrl}/edit/main/docs/${page.path}`} className="text-xs text-fd-muted-foreground hover:text-fd-foreground">Edit on GitHub</a>
      </div>
      <DocsBody><Content components={getMDXComponents({ a: createRelativeLink(source, page) })} /></DocsBody>
    </DocsPage>
  );
}

export function generateStaticParams() { return source.generateParams(); }

export async function generateMetadata(props: PageProps<'/[[...slug]]'>): Promise<Metadata> {
  const { slug } = await props.params;
  const page = source.getPage(slug);
  if (!page) notFound();
  const description = page.data.description ?? `${page.data.title} — Mango documentation.`;
  return {
    title: page.data.title,
    description,
    alternates: { canonical: absoluteUrl(page.url) },
    openGraph: { title: page.data.title, description, url: absoluteUrl(page.url), type: 'article' },
    twitter: { card: 'summary', title: page.data.title, description },
  };
}
