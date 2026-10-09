import http from 'node:http';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const root = fileURLToPath(new URL('../../internal/webui/static/', import.meta.url));
http
  .createServer((request, response) => {
    const pathname = new URL(request.url, 'http://localhost').pathname;
    const filename = path.resolve(root, '.' + (pathname === '/' ? '/index.html' : pathname));
    if (!filename.startsWith(root) || !fs.existsSync(filename) || !fs.statSync(filename).isFile()) {
      response.writeHead(404);
      response.end();
      return;
    }
    response.setHeader(
      'Content-Security-Policy',
      "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self'; img-src 'self' data:; connect-src 'self'; form-action 'self'; base-uri 'none'; object-src 'none'",
    );
    response.setHeader(
      'Content-Type',
      {
        '.html': 'text/html',
        '.js': 'text/javascript',
        '.css': 'text/css',
        '.svg': 'image/svg+xml',
      }[path.extname(filename)] || 'application/octet-stream',
    );
    response.end(fs.readFileSync(filename));
  })
  .listen(4178, '127.0.0.1');
