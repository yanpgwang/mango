import type { BaseLayoutProps } from 'fumadocs-ui/layouts/shared';
import { githubUrl, withBasePath } from './site.mjs';

export function baseOptions(): BaseLayoutProps {
  return {
    nav: {
      url: '/',
      title: (
        <span className="flex items-center gap-2.5">
          <img src={withBasePath('/img/mango-mark.svg')} alt="" width={29} height={29} />
          <span className="text-xl font-semibold tracking-tight">Mango</span>
          <span className="ml-1 text-xs font-normal text-fd-muted-foreground">Docs</span>
        </span>
      ),
    },
    githubUrl,
  };
}
