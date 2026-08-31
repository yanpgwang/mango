import defaultComponents from 'fumadocs-ui/mdx';
import type { MDXComponents } from 'mdx/types';
import { Tab, Tabs } from 'fumadocs-ui/components/tabs';
import { withBasePath } from '@/lib/site.mjs';
import { Mermaid } from './mermaid';

export function getMDXComponents(components?: MDXComponents): MDXComponents {
  return {
    ...defaultComponents,
    Tab,
    Tabs,
    Mermaid,
    img: ({ src, alt, ...props }) => (
      <img {...props} alt={alt ?? ''} src={typeof src === 'string' && src.startsWith('/') ? withBasePath(src) : src} />
    ),
    ...components,
  };
}

export const useMDXComponents = getMDXComponents;
