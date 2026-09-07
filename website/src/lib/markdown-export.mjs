import { defaultStringifier } from 'fumadocs-core/mdx-plugins/stringifier';
import { gfmToMarkdown } from 'mdast-util-gfm';

// Work on the compiled tree: included code is already expanded, and changing
// link destinations cannot accidentally rewrite examples inside code fences.
export function exportMarkdown(tree, resolveHref) {
  const root = structuredClone(tree);
  const visit = (node) => {
    if (['link', 'definition', 'image'].includes(node.type)) node.url = resolveHref(node.url);
    if (node.name === 'Card') {
      for (const attribute of node.attributes ?? []) {
        if (attribute.type === 'mdxJsxAttribute' && attribute.name === 'href' && typeof attribute.value === 'string') {
          attribute.value = resolveHref(attribute.value);
        }
      }
    }
    for (const child of node.children ?? []) visit(child);
  };
  visit(root);
  return defaultStringifier({
    extensions: [gfmToMarkdown()],
    filterElement: (node) => node.type !== 'mdxjsEsm',
  })(root);
}
