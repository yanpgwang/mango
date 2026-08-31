export function normalizeBasePath(value) {
  if (!value || value === '/') return '';
  if (!/^\/(?:[A-Za-z0-9_-]+\/?)+$/.test(value)) {
    throw new Error('DOCS_BASE_URL must be / or an absolute path such as /mango/');
  }
  return value.replace(/\/$/, '');
}

export const basePath = normalizeBasePath(
  process.env.NEXT_PUBLIC_DOCS_BASE_PATH ?? process.env.DOCS_BASE_URL ?? '/mango/',
);
export const siteOrigin = process.env.NEXT_PUBLIC_DOCS_ORIGIN ?? process.env.DOCS_URL ?? 'https://yanpgwang.github.io';
export const githubUrl = 'https://github.com/yanpgwang/mango';
export const withBasePath = (path) => `${basePath}${path.startsWith('/') ? path : `/${path}`}`;
export const absoluteUrl = (path) => new URL(withBasePath(path), siteOrigin).href;
