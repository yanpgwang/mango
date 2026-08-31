import { source } from '@/lib/source';
import { absoluteUrl } from '@/lib/site.mjs';
import { exportMarkdown } from '@/lib/markdown-export.mjs';

export const dynamic = 'force-static';
export const dynamicParams = false;

export function generateStaticParams() {
  return source.getPages().map((page) => ({ slug: [...page.slugs, 'index.md'] }));
}

export async function GET(_request: Request, context: { params: Promise<{ slug: string[] }> }) {
  const { slug } = await context.params;
  const page = source.getPage(slug.slice(0, -1));
  if (!page || slug.at(-1) !== 'index.md') return new Response('Not found', { status: 404 });
  const markdown = exportMarkdown(await page.data.getMDAST(), (href: string) => {
    const resolved = source.resolveHref(href, page);
    if (resolved.startsWith('#')) return absoluteUrl(`${page.url}${resolved}`);
    if (resolved.startsWith('/') && !resolved.startsWith('//')) return absoluteUrl(resolved);
    return resolved;
  });
  return new Response(`# ${page.data.title}\n\n${markdown}`, {
    headers: { 'Content-Type': 'text/markdown; charset=utf-8' },
  });
}
