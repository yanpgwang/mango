import { createMDX } from 'fumadocs-mdx/next';
import { fileURLToPath } from 'node:url';
import { basePath, siteOrigin } from './src/lib/site.mjs';

const withMDX = createMDX();

/** @type {import('next').NextConfig} */
const config = {
  output: 'export',
  trailingSlash: true,
  basePath,
  images: { unoptimized: true },
  reactStrictMode: true,
  turbopack: { root: fileURLToPath(new URL('../', import.meta.url)) },
  env: { NEXT_PUBLIC_DOCS_BASE_PATH: basePath, NEXT_PUBLIC_DOCS_ORIGIN: siteOrigin },
  agentRules: false,
};

export default withMDX(config);
