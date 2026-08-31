import { defineConfig, defineDocs } from 'fumadocs-mdx/config';
import { remarkDirectiveAdmonition, remarkMdxMermaid } from 'fumadocs-core/mdx-plugins';
import { pageSchema, metaSchema } from 'fumadocs-core/source/schema';
import remarkDirective from 'remark-directive';
import { z } from 'zod';
import { remarkDocumentTitle, remarkRelativeDocLinks } from './src/lib/markdown.mjs';

export const docs = defineDocs({
  dir: '../docs',
  docs: {
    schema: pageSchema.extend({ slug: z.string().optional() }),
    postprocess: { includeMDAST: true },
  },
  meta: { schema: metaSchema },
});

export default defineConfig({
  mdxOptions: {
    remarkImageOptions: { useImport: false, external: false },
    remarkPlugins: [remarkDirective, remarkDirectiveAdmonition, remarkMdxMermaid, remarkDocumentTitle, remarkRelativeDocLinks],
  },
});
