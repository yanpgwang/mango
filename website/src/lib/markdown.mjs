// Fumadocs resolves ./ and ../ file links, while ordinary Markdown also
// permits bare names such as sdk.md or api/sessions.md.
export function remarkRelativeDocLinks() {
  return (tree) => {
    const visit = (node) => {
      if ((node.type === 'link' || node.type === 'definition') &&
          typeof node.url === 'string' &&
          !/^(?:[a-z][a-z\d+.-]*:|\/|#|\.\.?\/)/i.test(node.url) &&
          /\.mdx?(?:[?#]|$)/i.test(node.url)) {
        node.url = `./${node.url}`;
      }
      for (const child of node.children ?? []) visit(child);
    };
    visit(tree);
  };
}

// Keep ordinary Markdown readable on GitHub; DocsTitle renders its H1 on the site.
export function remarkDocumentTitle() {
  return (tree) => {
    const first = tree.children.findIndex((node) => node.type !== 'yaml' && node.type !== 'mdxjsEsm');
    if (tree.children[first]?.type === 'heading' && tree.children[first].depth === 1) {
      tree.children.splice(first, 1);
    }
  };
}
