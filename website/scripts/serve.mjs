import { createServer } from 'node:http';
import { createReadStream } from 'node:fs';
import { stat } from 'node:fs/promises';
import { extname, join, resolve, sep } from 'node:path';
import { pathToFileURL } from 'node:url';
import { basePath } from '../src/lib/site.mjs';

const contentTypes = {
  '.html': 'text/html; charset=utf-8', '.css': 'text/css; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8', '.json': 'application/json',
  '.svg': 'image/svg+xml', '.gif': 'image/gif', '.png': 'image/png',
  '.woff2': 'font/woff2', '.txt': 'text/plain; charset=utf-8',
  '.md': 'text/markdown; charset=utf-8',
};

// A local static-only preview, not a production application server. In
// particular, unknown URLs must return 404 rather than an SPA index fallback.
export function createPreviewServer(directory, mount = basePath) {
  const root = resolve(directory);
  return createServer(async (request, response) => {
    if (request.method !== 'GET' && request.method !== 'HEAD') {
      response.writeHead(405).end();
      return;
    }
    try {
      const pathname = decodeURIComponent(new URL(request.url, 'http://localhost').pathname);
      if (mount && pathname !== mount && !pathname.startsWith(`${mount}/`)) {
        response.writeHead(404).end('Not found');
        return;
      }
      const relative = pathname.slice(mount.length).replace(/^\/+/, '');
      let file = resolve(root, relative);
      if (file !== root && !file.startsWith(root + sep)) {
        response.writeHead(404).end('Not found');
        return;
      }
      let info = await stat(file);
      if (info.isDirectory()) {
        file = join(file, 'index.html');
        info = await stat(file);
      }
      if (!info.isFile()) throw new Error('Not a file');
      response.writeHead(200, {
        'Content-Type': contentTypes[extname(file)] ?? 'application/octet-stream',
        'Content-Length': info.size,
        'Cache-Control': 'no-cache',
        'X-Content-Type-Options': 'nosniff',
      });
      if (request.method === 'HEAD') response.end();
      else createReadStream(file).on('error', () => response.destroy()).pipe(response);
    } catch {
      response.writeHead(404).end('Not found');
    }
  });
}

if (process.argv[1] && pathToFileURL(resolve(process.argv[1])).href === import.meta.url) {
  const port = Number(process.env.DOCS_PORT ?? 4175);
  if (!Number.isInteger(port) || port < 1 || port > 65535) throw new Error('Invalid DOCS_PORT');
  const server = createPreviewServer('out');
  server.listen(port, '127.0.0.1', () => {
    console.log(`Static docs preview: http://127.0.0.1:${port}${basePath}/`);
  });
}
