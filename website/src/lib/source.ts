import { docs } from '../../.source/server';
import { loader } from 'fumadocs-core/source';
import { slugsFromData } from 'fumadocs-core/source/plugins/slugs';

export const source = loader({
  baseUrl: '/',
  slugs: slugsFromData(),
  source: docs.toFumadocsSource(),
});

export type DocPage = (typeof source)['$inferPage'];
export const markdownPath = (page: DocPage) => `/markdown/${[...page.slugs, 'index.md'].join('/')}`;
