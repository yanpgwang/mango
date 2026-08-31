'use client';

import { useEffect, useId, useState } from 'react';

export function Mermaid({ chart }: { chart: string }) {
  const id = useId().replace(/[^a-zA-Z0-9_-]/g, '');
  const [svg, setSvg] = useState('');
  const [error, setError] = useState(false);
  useEffect(() => {
    let cancelled = false;
    import('mermaid').then(async ({ default: mermaid }) => {
      if (cancelled) return;
      setError(false);
      mermaid.initialize({ startOnLoad: false, securityLevel: 'strict', theme: 'neutral' });
      const rendered = await mermaid.render(`mango-${id}`, chart);
      if (!cancelled) setSvg(rendered.svg);
    }).catch(() => { if (!cancelled) setError(true); });
    return () => { cancelled = true; };
  }, [chart, id]);
  if (!svg || error) return <pre className="overflow-auto"><code>{chart}</code></pre>;
  // Mermaid's strict security mode sanitizes repository-authored diagrams.
  return <div className="my-6 overflow-auto rounded-lg bg-white p-4" role="img" aria-label="Architecture diagram" dangerouslySetInnerHTML={{ __html: svg }} />;
}
