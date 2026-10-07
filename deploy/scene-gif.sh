#!/bin/bash
#
# Make docs/scene-wifi.gif, the README's picture of the site's first scene.
#
# The GIF is a picture of site/index.html, so it can go stale the way any
# picture can: run this after the scene or its words change. Frames are drawn
# with the scene engine's own render(t), one per eighth of a second, with
# transitions off so each frame is exactly that moment. Playwright's image
# draws them and ffmpeg, on this machine, makes the GIF.
set -euo pipefail
cd "$(dirname "$0")/.."

PLAYWRIGHT=mcr.microsoft.com/playwright:v1.63.0-noble
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

cat > "$work/frames.js" <<'JS'
const { chromium } = require('playwright');
const http = require('http'), fs = require('fs'), path = require('path');
const types = { '.html': 'text/html', '.css': 'text/css', '.js': 'text/javascript' };
const srv = http.createServer((q, r) => {
  const f = path.join('/site', q.url === '/' ? 'index.html' : q.url);
  fs.readFile(f, (e, d) => { if (e) { r.writeHead(404); return r.end(); } r.writeHead(200, { 'Content-Type': types[path.extname(f)] }); r.end(d); });
}).listen(8099);
(async () => {
  const b = await chromium.launch();
  // The page's own policy forbids injected styles; this is a tool, not a visitor.
  const p = await b.newPage({ viewport: { width: 1000, height: 700 }, reducedMotion: 'reduce', bypassCSP: true });
  await p.goto('http://localhost:8099/');
  await p.addStyleTag({ content: '*{transition:none!important;animation:none!important}' });
  const wrap = p.locator('.an-wrap').first();
  await wrap.scrollIntoViewIfNeeded();
  const fps = 8, loop = await wrap.evaluate((w) => parseFloat(w.dataset.loop));
  for (let i = 0; i < loop * fps; i++) {
    await wrap.evaluate((w, t) => w.__anRender(t), i / fps);
    await wrap.screenshot({ path: `/out/f${String(i).padStart(4, '0')}.png` });
  }
  await b.close(); srv.close();
})();
JS

docker run --rm -v "$PWD/site":/site:ro -v "$work":/out -w /tmp "$PLAYWRIGHT" \
  bash -c 'npm i -s playwright@1.63.0 >/dev/null 2>&1 && cp /out/frames.js . && node frames.js'
ffmpeg -loglevel error -y -framerate 8 -i "$work/f%04d.png" \
  -vf "scale=800:-1:flags=lanczos,split[a][b];[a]palettegen=max_colors=96:stats_mode=diff[p];[b][p]paletteuse=dither=bayer:bayer_scale=4:diff_mode=rectangle" \
  -loop 0 docs/scene-wifi.gif
ls -la docs/scene-wifi.gif
