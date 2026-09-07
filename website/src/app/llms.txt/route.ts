import { source, markdownPath } from '@/lib/source';
import { absoluteUrl } from '@/lib/site.mjs';

export const dynamic = 'force-static';
export function GET() {
  return new Response([
    '# Mango',
    '',
    '> The open-source, self-hosted alternative to Claude Managed Agents.',
    '',
    ...source.getPages().map((page) => `- [${page.data.title}](${absoluteUrl(markdownPath(page))})`),
  ].join('\n'), { headers: { 'Content-Type': 'text/plain; charset=utf-8' } });
}
